package api

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"katchup/internal/account"
	acctView "katchup/internal/view/account"
)

type AccountHandler struct {
	store *account.Store
}

func NewAccountHandler(store *account.Store) *AccountHandler {
	return &AccountHandler{store: store}
}

func (h *AccountHandler) List(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()

	accounts, err := h.store.ListAccounts(ctx)
	if err != nil {
		slog.Error("failed to list accounts", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	var views []acctView.AccountView
	for _, a := range accounts {
		views = append(views, acctView.AccountView{
			ID:        a.ID,
			Name:      a.Name,
			Host:      a.Host,
			Port:      a.Port,
			Username:  a.Username,
			UseSsl:    a.UseSsl,
			Folders:   strings.Join(a.Folders, ", "),
			CreatedAt: a.CreatedAt,
			UpdatedAt: a.UpdatedAt,
			IsSyncing: a.IsSyncing,
		})
	}

	data := acctView.PageData{
		Title:    "Accounts",
		Accounts: views,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := acctView.ListPage(data).Render(ctx, w); err != nil {
		slog.Error("failed to render account list", "error", err)
	}
}

func (h *AccountHandler) NewForm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	data := acctView.PageData{
		Title: "Add Account",
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := acctView.FormPage(data).Render(r.Context(), w); err != nil {
		slog.Error("failed to render new account form", "error", err)
	}
}

func (h *AccountHandler) Create(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	host := strings.TrimSpace(r.FormValue("host"))
	portStr := strings.TrimSpace(r.FormValue("port"))
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	connection := strings.TrimSpace(r.FormValue("connection"))
	foldersStr := strings.TrimSpace(r.FormValue("folders"))

	// Validate required fields
	var errs []string
	if name == "" {
		errs = append(errs, "Name is required")
	}
	if host == "" {
		errs = append(errs, "Host is required")
	}
	if username == "" {
		errs = append(errs, "Username is required")
	}
	if password == "" {
		errs = append(errs, "Password is required")
	}

	// Parse port: use the actual port from the form value
	// or connection selection, not a default of 993.
	port := int64(143) // default to plaintext unless overridden
	if connection != "" {
		port = parsePort(connection)
	}
	if portStr != "" {
		p, err := strconv.ParseInt(portStr, 10, 64)
		if err != nil {
			errs = append(errs, "Invalid port number")
		} else {
			port = p
		}
	}

	// Derive useSsl from the port: 993 = implicit SSL/TLS, 143 = STARTTLS.
	useSsl := port == 993

	// Parse folders
	var folders []string
	if foldersStr != "" {
		for _, f := range strings.Split(foldersStr, ",") {
			f = strings.TrimSpace(f)
			if f != "" {
				folders = append(folders, f)
			}
		}
	}
	if len(folders) == 0 {
		folders = []string{"INBOX"}
	}
	for _, f := range folders {
		if invalidFolderName(f) {
			errs = append(errs, "Invalid folder name: "+f)
			break
		}
	}

	if len(errs) > 0 {
		data := acctView.PageData{
			Title:  "Add Account",
			Errors: errs,
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := acctView.FormPage(data).Render(r.Context(), w); err != nil {
			slog.Error("failed to render form with errors", "error", err)
		}
		return
	}

	// Encrypt password only if master key is configured.
	// Without a master key, store the password as plaintext (no encryption).
	var encryptedPass string
	var encErr error
	if h.store.MasterKeySet() {
		encryptedPass, encErr = h.store.EncryptPassword(password)
		if encErr != nil {
			slog.Error("failed to encrypt password", "error", encErr)
			http.Error(w, "encryption error", http.StatusInternalServerError)
			return
		}
	} else {
		encryptedPass = password
	}

	ctx := r.Context()
	_, err := h.store.CreateAccount(ctx, name, host, port, username, encryptedPass, useSsl, folders)
	if err != nil {
		slog.Error("failed to create account", "error", err)
		errs = []string{"Failed to create account: " + err.Error()}
		data := acctView.PageData{
			Title:  "Add Account",
			Errors: errs,
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := acctView.FormPage(data).Render(ctx, w); err != nil {
			slog.Error("failed to render form with error", "error", err)
		}
		return
	}

	slog.Info("created account", "name", name, "host", host)
	http.Redirect(w, r, "/accounts", http.StatusSeeOther)
}

func (h *AccountHandler) EditForm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	idStr := extractID(r.URL.Path, "/accounts/")
	if idStr == "" {
		http.NotFound(w, r)
		return
	}

	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	ctx := r.Context()
	acct, err := h.store.GetAccount(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		slog.Error("failed to get account", "id", id, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	view := acctView.AccountView{
		ID:        acct.ID,
		Name:      acct.Name,
		Host:      acct.Host,
		Port:      acct.Port,
		Username:  acct.Username,
		UseSsl:    acct.UseSsl,
		Folders:   strings.Join(acct.Folders, ", "),
		CreatedAt: acct.CreatedAt,
		UpdatedAt: acct.UpdatedAt,
	}

	data := acctView.PageData{
		Title:   "Edit Account",
		Account: &view,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := acctView.FormPage(data).Render(ctx, w); err != nil {
		slog.Error("failed to render edit form", "error", err)
	}
}

func (h *AccountHandler) Update(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	idStr := extractID(r.URL.Path, "/accounts/")
	if idStr == "" {
		http.NotFound(w, r)
		return
	}

	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	host := strings.TrimSpace(r.FormValue("host"))
	portStr := strings.TrimSpace(r.FormValue("port"))
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	connection := strings.TrimSpace(r.FormValue("connection"))
	foldersStr := strings.TrimSpace(r.FormValue("folders"))

	// Get existing account to check fields
	ctx := r.Context()
	existing, err := h.store.GetAccount(ctx, id)
	if err != nil {
		if err == sql.ErrNoRows {
			http.NotFound(w, r)
			return
		}
		slog.Error("failed to get account", "id", id, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// Validate required fields
	var errs []string
	if name == "" {
		errs = append(errs, "Name is required")
	}
	if host == "" {
		errs = append(errs, "Host is required")
	}
	if username == "" {
		errs = append(errs, "Username is required")
	}
	// Password is optional on update — blank keeps the stored one.

	// Parse port — default to 143 (STARTTLS), not 993.
	port := int64(143)
	if connection != "" {
		port = parsePort(connection)
	}
	if portStr != "" {
		p, err := strconv.ParseInt(portStr, 10, 64)
		if err != nil {
			errs = append(errs, "Invalid port number")
		} else {
			port = p
		}
	}

	useSsl := port == 993

	// Parse folders
	var folders []string
	if foldersStr != "" {
		for _, f := range strings.Split(foldersStr, ",") {
			f = strings.TrimSpace(f)
			if f != "" {
				folders = append(folders, f)
			}
		}
	}
	if len(folders) == 0 {
		folders = []string{"INBOX"}
	}
	for _, f := range folders {
		if invalidFolderName(f) {
			errs = append(errs, "Invalid folder name: "+f)
			break
		}
	}

	if len(errs) > 0 {
		view := acctView.AccountView{
			ID:        existing.ID,
			Name:      existing.Name,
			Host:      existing.Host,
			Port:      existing.Port,
			Username:  existing.Username,
			UseSsl:    existing.UseSsl,
			Folders:   strings.Join(existing.Folders, ", "),
			CreatedAt: existing.CreatedAt,
			UpdatedAt: existing.UpdatedAt,
		}
		data := acctView.PageData{
			Title:   "Edit Account",
			Account: &view,
			Errors:  errs,
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := acctView.FormPage(data).Render(r.Context(), w); err != nil {
			slog.Error("failed to render form with errors", "error", err)
		}
		return
	}

	// Only encrypt and use new password if provided.
	// Otherwise, keep the existing encrypted password.
	encryptedPass := existing.EncryptedPassword
	if password != "" {
		if h.store.MasterKeySet() {
			var err error
			encryptedPass, err = h.store.EncryptPassword(password)
			if err != nil {
				slog.Error("failed to encrypt password", "error", err)
				http.Error(w, "encryption error", http.StatusInternalServerError)
				return
			}
		} else {
			encryptedPass = password
		}
	}

	_, err = h.store.UpdateAccount(ctx, id, name, host, port, username, encryptedPass, useSsl, folders)
	if err != nil {
		slog.Error("failed to update account", "id", id, "error", err)
		errs = []string{"Failed to update account: " + err.Error()}
		view := acctView.AccountView{
			ID:        existing.ID,
			Name:      existing.Name,
			Host:      existing.Host,
			Port:      existing.Port,
			Username:  existing.Username,
			UseSsl:    existing.UseSsl,
			Folders:   strings.Join(existing.Folders, ", "),
			CreatedAt: existing.CreatedAt,
			UpdatedAt: existing.UpdatedAt,
		}
		data := acctView.PageData{
			Title:   "Edit Account",
			Account: &view,
			Errors:  errs,
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := acctView.FormPage(data).Render(r.Context(), w); err != nil {
			slog.Error("failed to render form with error", "error", err)
		}
		return
	}

	slog.Info("updated account", "id", id, "name", name)
	http.Redirect(w, r, "/accounts", http.StatusSeeOther)
}

func (h *AccountHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	idStr := extractID(r.URL.Path, "/accounts/")
	if idStr == "" {
		http.NotFound(w, r)
		return
	}

	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	ctx := r.Context()

	// Check if account exists before deleting
	_, err = h.store.GetAccount(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		slog.Error("failed to get account for delete", "id", id, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if err := h.store.DeleteAccount(ctx, id); err != nil {
		slog.Error("failed to delete account", "id", id, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	slog.Info("deleted account", "id", id)
	http.Redirect(w, r, "/accounts", http.StatusSeeOther)
}

// invalidFolderName rejects folder values that could escape the data directory
// when the folder is used as an on-disk path segment ("..", absolute paths,
// backslashes). IMAP hierarchy names like "[Gmail]/All Mail" stay valid.
func invalidFolderName(f string) bool {
	if strings.HasPrefix(f, "/") || strings.Contains(f, "\\") || strings.ContainsRune(f, 0) {
		return true
	}
	for _, seg := range strings.Split(f, "/") {
		if seg == ".." || seg == "." {
			return true
		}
	}
	return false
}

// parsePort converts a connection value to a port number.
func parsePort(conn string) int64 {
	switch conn {
	case "993":
		return 993
	case "143":
		return 143
	default:
		return 993
	}
}

// extractID extracts the account ID from a URL path.
func extractID(path, prefix string) string {
	path = strings.TrimPrefix(path, prefix)
	path = strings.TrimSuffix(path, "/delete")
	path = strings.TrimSuffix(path, "/edit")
	return path
}

// routeAccount handles the accounts routing by dispatching to the correct handler.
func (h *AccountHandler) routeAccount(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	switch {
	case path == "/accounts" && r.Method == http.MethodGet:
		h.List(w, r)
	case path == "/accounts/new" && r.Method == http.MethodGet:
		h.NewForm(w, r)
	case path == "/accounts/new" && r.Method == http.MethodPost:
		h.Create(w, r)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/edit") && r.Method == http.MethodGet:
		h.EditForm(w, r)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/edit") && r.Method == http.MethodPost:
		h.Update(w, r)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/delete") && r.Method == http.MethodPost:
		h.Delete(w, r)
	default:
		http.NotFound(w, r)
	}
}

// ServeHTTP makes AccountHandler implement http.Handler.
func (h *AccountHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.routeAccount(w, r)
}

// formatTime formats a time string for display (abbreviated).
func formatTime(t string) string {
	if t == "" {
		return "—"
	}
	// Already in a reasonable format from the database
	return t
}

// statusBadge returns the status HTML for an account.
func statusBadge(syncing bool, updatedAt string) string {
	if syncing {
		return fmt.Sprintf(`<span class="inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-medium">
			<span class="w-2 h-2 mr-1 rounded-full bg-green-500"></span>
			Running
		</span>`)
	}
	if updatedAt != "" {
		return fmt.Sprintf(`<span class="inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-medium">
			<span class="w-2 h-2 mr-1 rounded-full bg-gray-400"></span>
			Synced
		</span>`)
	}
	return fmt.Sprintf(`<span class="inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-medium">
		<span class="w-2 h-2 mr-1 rounded-full bg-gray-400"></span>
		Never
	</span>`)
}
