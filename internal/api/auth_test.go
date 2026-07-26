package api

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func newTestUIAuth(t *testing.T, envKey string) *UIAuth {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "auth-test.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.Exec(`
		CREATE TABLE app_settings (
			key        TEXT PRIMARY KEY,
			value      TEXT NOT NULL,
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`); err != nil {
		t.Fatalf("create app_settings: %v", err)
	}

	return NewUIAuth(db, envKey)
}

// protectedMux returns a mux with the auth routes plus a marker route so tests
// can tell whether the middleware let a request through.
func protectedMux(a *UIAuth) http.Handler {
	mux := http.NewServeMux()
	a.RegisterRoutes(mux)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("dashboard"))
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return a.Middleware(mux)
}

func postForm(h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func sessionFrom(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return c
		}
	}
	t.Fatal("expected a session cookie")
	return nil
}

func TestUIAuth_RedirectsAnonymousToLogin(t *testing.T) {
	h := protectedMux(newTestUIAuth(t, "env-key"))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("expected 303 to /login, got %d to %q", w.Code, w.Header().Get("Location"))
	}
}

func TestUIAuth_HealthAndAPIExempt(t *testing.T) {
	a := newTestUIAuth(t, "env-key")
	mux := http.NewServeMux()
	a.RegisterRoutes(mux)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /api/archived", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /static/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := a.Middleware(mux)

	for _, path := range []string{"/health", "/api/archived", "/static/tailwind.js"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected %s to bypass UI auth, got %d", path, w.Code)
		}
	}
}

func TestUIAuth_EnvKeyLoginFlow(t *testing.T) {
	h := protectedMux(newTestUIAuth(t, "correct-horse"))

	// Wrong key: re-rendered login page (200), no session cookie.
	w := postForm(h, "/login", url.Values{"key": {"wrong"}})
	if w.Code != http.StatusOK {
		t.Fatalf("expected re-rendered login on wrong key, got %d", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.Fatal("wrong key must not mint a session")
		}
	}

	// Correct key: redirect home with a session cookie.
	w = postForm(h, "/login", url.Values{"key": {"correct-horse"}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("expected 303 to /, got %d to %q", w.Code, w.Header().Get("Location"))
	}
	cookie := sessionFrom(t, w)
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie must be HttpOnly + SameSite=Lax, got %+v", cookie)
	}

	// The session unlocks the protected route.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with session, got %d", rec.Code)
	}

	// Logout invalidates the session.
	reqOut := httptest.NewRequest(http.MethodPost, "/logout", nil)
	reqOut.AddCookie(cookie)
	recOut := httptest.NewRecorder()
	h.ServeHTTP(recOut, reqOut)
	if recOut.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 from logout, got %d", recOut.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected redirect after logout, got %d", rec.Code)
	}
}

func TestUIAuth_FirstRunSetupFlow(t *testing.T) {
	h := protectedMux(newTestUIAuth(t, ""))

	// Mismatched confirmation is rejected.
	w := postForm(h, "/login", url.Values{
		"mode": {"setup"}, "key": {"hunter2hunter2"}, "confirm": {"different"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected re-rendered setup page, got %d", w.Code)
	}

	// Too-short key is rejected.
	w = postForm(h, "/login", url.Values{
		"mode": {"setup"}, "key": {"short"}, "confirm": {"short"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected re-rendered setup page for short key, got %d", w.Code)
	}

	// Valid setup mints a session.
	w = postForm(h, "/login", url.Values{
		"mode": {"setup"}, "key": {"hunter2hunter2"}, "confirm": {"hunter2hunter2"},
	})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 after setup, got %d", w.Code)
	}
	sessionFrom(t, w)

	// A second setup attempt is refused (no session cookie).
	w = postForm(h, "/login", url.Values{
		"mode": {"setup"}, "key": {"attacker-key-123"}, "confirm": {"attacker-key-123"},
	})
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.Fatal("second setup must not mint a session")
		}
	}

	// The stored key now logs in.
	w = postForm(h, "/login", url.Values{"key": {"hunter2hunter2"}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 logging in with stored key, got %d", w.Code)
	}

	// The attacker's key does not.
	w = postForm(h, "/login", url.Values{"key": {"attacker-key-123"}})
	if w.Code != http.StatusOK {
		t.Fatalf("expected login page for rejected key, got %d", w.Code)
	}
}

func TestHashUIKey_RoundTripAndTamper(t *testing.T) {
	hash, err := hashUIKey("some-access-key")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !verifyUIKey(hash, "some-access-key") {
		t.Fatal("correct key must verify")
	}
	if verifyUIKey(hash, "some-access-kez") {
		t.Fatal("wrong key must not verify")
	}
	if verifyUIKey("garbage", "some-access-key") {
		t.Fatal("malformed stored hash must not verify")
	}
}
