package api

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	loginView "katchup/internal/view/login"
)

const (
	sessionCookie  = "katchup_session"
	sessionTTL     = 30 * 24 * time.Hour
	pbkdf2Iters    = 600_000 // OWASP guidance for PBKDF2-HMAC-SHA256
	minUIKeyLen    = 8
	loginFailDelay = 500 * time.Millisecond
	uiKeySetting   = "ui_key_hash"
	// maxConcurrentHashes caps simultaneous PBKDF2 verifications so a flood of
	// login attempts can't occupy every core.
	maxConcurrentHashes = 2
)

// UIAuth guards the web UI behind an access key. The key comes from
// KATCHUP_UI_KEY when set; otherwise the first visit shows a setup page and the
// key's PBKDF2 hash is stored in app_settings. Setup requires a one-time token
// printed to the server log at startup, so whoever reaches the port first
// can't claim the UI (nor can a cross-site form post), and losing the DB
// doesn't reopen setup to anyone without log access. Sessions are random tokens held
// in memory (a restart logs everyone out) and carried in an HttpOnly,
// SameSite=Lax cookie — which also stops cross-site request forgery against the
// state-changing POST routes, since cross-site POSTs never carry the cookie.
//
// Login attempts are rate-limited per client address (see loginLimiter), and
// at most maxConcurrentHashes PBKDF2 checks run at once.
type UIAuth struct {
	db      *sql.DB
	envKey  string
	limiter *loginLimiter
	hashSem chan struct{}

	mu         sync.Mutex
	sessions   map[string]time.Time // token → expiry
	setupToken string               // while no key is configured; see setupTokenFor
}

func NewUIAuth(db *sql.DB, envKey string) *UIAuth {
	a := &UIAuth{
		db:       db,
		envKey:   envKey,
		limiter:  newLoginLimiter(),
		hashSem:  make(chan struct{}, maxConcurrentHashes),
		sessions: make(map[string]time.Time),
	}
	// Print the setup token at startup so it is in the log before anyone
	// visits.
	if configured, err := a.keyConfigured(context.Background()); err == nil && !configured {
		a.setupTokenFor()
	}
	return a
}

// setupTokenFor returns the one-time first-run setup token, generating and
// logging it on first use. It lives only in memory: a restart prints a new one.
func (a *UIAuth) setupTokenFor() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.setupToken == "" {
		buf := make([]byte, 12)
		if _, err := rand.Read(buf); err != nil {
			slog.Error("ui auth: setup token generation failed", "error", err)
			return ""
		}
		a.setupToken = hex.EncodeToString(buf)
		slog.Warn("ui auth: no access key configured — open /login and enter this setup token to create one",
			"setup_token", a.setupToken)
	}
	return a.setupToken
}

// RegisterRoutes adds the login/logout endpoints to the mux. The middleware
// exempts /login so these are reachable before authentication.
func (a *UIAuth) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", a.loginForm)
	mux.HandleFunc("POST /login", a.login)
	mux.HandleFunc("POST /logout", a.logout)
}

// Middleware requires a valid session for every route except /health (probes),
// /api/* (token-authenticated separately) and /login itself.
func (a *UIAuth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/health" || p == "/login" ||
			strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		if a.validSession(r) {
			next.ServeHTTP(w, r)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

func (a *UIAuth) loginForm(w http.ResponseWriter, r *http.Request) {
	if a.validSession(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.renderLogin(w, r, "")
}

func (a *UIAuth) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if wait, ok := a.limiter.allow(ip); !ok {
		slog.Warn("ui auth: login attempt while locked out", "remote", r.RemoteAddr)
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		a.renderLoginStatus(w, r, http.StatusTooManyRequests,
			fmt.Sprintf("Too many failed attempts. Try again in %d minutes.", int(wait.Minutes())+1))
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if r.FormValue("mode") == "setup" {
		a.setup(w, r)
		return
	}

	key := r.FormValue("key")
	ok, err := a.verify(r.Context(), key)
	if err != nil {
		slog.Error("ui auth: verify failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if !ok {
		a.limiter.fail(ip)
		// Flat delay keeps online guessing slow; the compare itself is
		// constant-time.
		time.Sleep(loginFailDelay)
		slog.Warn("ui auth: failed login attempt", "remote", r.RemoteAddr)
		a.renderLogin(w, r, "Wrong access key.")
		return
	}

	a.limiter.success(ip)
	a.issueSession(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// setup handles the first-run "create access key" POST. It only succeeds while
// no key is configured; the INSERT (not upsert) makes concurrent setups safe —
// the second one fails on the primary key and is turned away.
func (a *UIAuth) setup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	configured, err := a.keyConfigured(ctx)
	if err != nil {
		slog.Error("ui auth: settings lookup failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if configured {
		a.renderLogin(w, r, "An access key is already configured.")
		return
	}

	want := a.setupTokenFor()
	if want == "" || subtle.ConstantTimeCompare([]byte(want), []byte(strings.TrimSpace(r.FormValue("token")))) != 1 {
		a.limiter.fail(clientIP(r))
		time.Sleep(loginFailDelay)
		slog.Warn("ui auth: setup attempt with a wrong setup token", "remote", r.RemoteAddr)
		a.renderLogin(w, r, "Wrong setup token — it is printed in the server log.")
		return
	}

	key := r.FormValue("key")
	if len(key) < minUIKeyLen {
		a.renderLogin(w, r, fmt.Sprintf("Access key must be at least %d characters.", minUIKeyLen))
		return
	}
	if key != r.FormValue("confirm") {
		a.renderLogin(w, r, "Keys do not match.")
		return
	}

	hash, err := hashUIKey(key)
	if err != nil {
		slog.Error("ui auth: hashing failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	_, err = a.db.ExecContext(ctx,
		"INSERT INTO app_settings (key, value) VALUES (?1, ?2)", uiKeySetting, hash)
	if err != nil {
		// Most likely a concurrent setup won the INSERT race.
		slog.Warn("ui auth: storing access key failed", "error", err)
		a.renderLogin(w, r, "An access key is already configured.")
		return
	}

	slog.Info("ui auth: access key created via first-run setup")
	a.mu.Lock()
	a.setupToken = ""
	a.mu.Unlock()
	a.issueSession(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *UIAuth) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (a *UIAuth) renderLogin(w http.ResponseWriter, r *http.Request, errMsg string) {
	a.renderLoginStatus(w, r, http.StatusOK, errMsg)
}

func (a *UIAuth) renderLoginStatus(w http.ResponseWriter, r *http.Request, code int, errMsg string) {
	configured, err := a.keyConfigured(r.Context())
	if err != nil {
		slog.Error("ui auth: settings lookup failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	data := loginView.PageData{Setup: !configured, Error: errMsg}
	if err := loginView.LoginPage(data).Render(r.Context(), w); err != nil {
		slog.Error("ui auth: failed to render login page", "error", err)
	}
}

// keyConfigured reports whether any access key exists (environment or stored).
func (a *UIAuth) keyConfigured(ctx context.Context) (bool, error) {
	if a.envKey != "" {
		return true, nil
	}
	_, found, err := a.storedHash(ctx)
	return found, err
}

func (a *UIAuth) storedHash(ctx context.Context) (string, bool, error) {
	var hash string
	err := a.db.QueryRowContext(ctx,
		"SELECT value FROM app_settings WHERE key = ?1", uiKeySetting).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return hash, true, nil
}

// verify checks a presented key. KATCHUP_UI_KEY takes precedence over a stored
// hash so an operator can always regain access via the environment.
func (a *UIAuth) verify(ctx context.Context, key string) (bool, error) {
	if key == "" {
		return false, nil
	}
	if a.envKey != "" {
		return subtle.ConstantTimeCompare([]byte(a.envKey), []byte(key)) == 1, nil
	}
	hash, found, err := a.storedHash(ctx)
	if err != nil || !found {
		return false, err
	}
	select {
	case a.hashSem <- struct{}{}:
		defer func() { <-a.hashSem }()
	case <-ctx.Done():
		return false, ctx.Err()
	}
	return verifyUIKey(hash, key), nil
}

func (a *UIAuth) issueSession(w http.ResponseWriter) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// rand.Read failing means the system CSPRNG is broken; don't mint a
		// guessable session, just leave the user unauthenticated.
		slog.Error("ui auth: session token generation failed", "error", err)
		return
	}
	token := hex.EncodeToString(buf)

	a.mu.Lock()
	now := time.Now()
	for t, exp := range a.sessions {
		if now.After(exp) {
			delete(a.sessions, t)
		}
	}
	a.sessions[token] = now.Add(sessionTTL)
	a.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		// Lax blocks cross-site POSTs (CSRF) while keeping normal navigation
		// working. Secure is not set because the target deployment is plain
		// HTTP on a trusted LAN; a TLS reverse proxy upgrade is compatible.
		SameSite: http.SameSiteLaxMode,
	})
}

func (a *UIAuth) validSession(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[c.Value]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(a.sessions, c.Value)
		return false
	}
	return true
}

// hashUIKey returns "pbkdf2:sha256:<iters>:<salt hex>:<dk hex>".
func hashUIKey(key string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	dk, err := pbkdf2.Key(sha256.New, key, salt, pbkdf2Iters, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2:sha256:%d:%s:%s",
		pbkdf2Iters, hex.EncodeToString(salt), hex.EncodeToString(dk)), nil
}

// verifyUIKey recomputes the PBKDF2 hash with the stored parameters and
// compares in constant time.
func verifyUIKey(stored, key string) bool {
	parts := strings.Split(stored, ":")
	if len(parts) != 5 || parts[0] != "pbkdf2" || parts[1] != "sha256" {
		return false
	}
	iters, err := strconv.Atoi(parts[2])
	if err != nil || iters <= 0 {
		return false
	}
	salt, err := hex.DecodeString(parts[3])
	if err != nil {
		return false
	}
	want, err := hex.DecodeString(parts[4])
	if err != nil {
		return false
	}
	dk, err := pbkdf2.Key(sha256.New, key, salt, iters, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(dk, want) == 1
}
