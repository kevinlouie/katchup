package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"katchup/internal/account"
	migrations "katchup/sql/migrations"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

func newTestHandler(t *testing.T) (*AccountHandler, func()) {
	t.Helper()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatalf("enable WAL: %v", err)
	}

	if err := createTestTables(db); err != nil {
		t.Fatalf("create tables: %v", err)
	}

	store, err := account.New(db, "test-master-key-for-http-tests")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	handler := NewAccountHandler(store)

	return handler, func() {
		db.Close()
		os.RemoveAll(dir)
	}
}

// createTestTables applies the real (embedded) migrations so handler tests
// exercise the production schema rather than a hand-maintained copy.
func createTestTables(db *sql.DB) error {
	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		return err
	}
	return goose.Up(db, ".")
}

func TestListAccounts(t *testing.T) {
	handler, cleanup := newTestHandler(t)
	defer cleanup()

	ctx := context.Background()
	_, err := handler.store.CreateAccount(ctx, "Test Account", "imap.test.com", 993, "test@test.com", "encrypted-pass", true, []string{"INBOX"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/accounts", nil)
	w := httptest.NewRecorder()
	handler.List(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "Test Account") {
		t.Error("expected response to contain 'Test Account'")
	}
	if !strings.Contains(body, "imap.test.com") {
		t.Error("expected response to contain 'imap.test.com'")
	}
}

func TestListAccountsEmpty(t *testing.T) {
	handler, cleanup := newTestHandler(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/accounts", nil)
	w := httptest.NewRecorder()
	handler.List(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "No mailboxes yet") {
		t.Error("expected response to contain 'No mailboxes yet'")
	}
}

func TestListAccountsWrongMethod(t *testing.T) {
	handler, cleanup := newTestHandler(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodPost, "/accounts", nil)
	w := httptest.NewRecorder()
	handler.List(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status 405, got %d", w.Code)
	}
}

func TestNewForm(t *testing.T) {
	handler, cleanup := newTestHandler(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/accounts/new", nil)
	w := httptest.NewRecorder()
	handler.NewForm(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "Add Account") {
		t.Error("expected response to contain 'Add Account'")
	}
	if !strings.Contains(body, "name") {
		t.Error("expected response to contain 'name' field")
	}
	if !strings.Contains(body, "host") {
		t.Error("expected response to contain 'host' field")
	}
}

func TestNewFormWrongMethod(t *testing.T) {
	handler, cleanup := newTestHandler(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodPost, "/accounts/new", nil)
	w := httptest.NewRecorder()
	handler.NewForm(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status 405, got %d", w.Code)
	}
}

func TestCreateAccount(t *testing.T) {
	handler, cleanup := newTestHandler(t)
	defer cleanup()

	form := url.Values{}
	form.Set("name", "New Account")
	form.Set("host", "imap.new.com")
	form.Set("port", "993")
	form.Set("username", "new@test.com")
	form.Set("password", "secret123")
	form.Set("connection", "993")
	form.Set("folders", "INBOX, Sent")

	req := httptest.NewRequest(http.MethodPost, "/accounts/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.Create(w, req)

	if w.Code != http.StatusSeeOther {
		t.Errorf("expected status 303, got %d", w.Code)
	}

	loc := w.Header().Get("Location")
	if loc != "/accounts" {
		t.Errorf("expected redirect to /accounts, got %q", loc)
	}
}

func TestCreateAccountValidationErrors(t *testing.T) {
	handler, cleanup := newTestHandler(t)
	defer cleanup()

	tests := []struct {
		name     string
		form     url.Values
		expected string
	}{
		{
			name: "missing name",
			form: func() url.Values {
				v := url.Values{}
				v.Set("name", "")
				v.Set("host", "imap.test.com")
				v.Set("port", "993")
				v.Set("username", "test@test.com")
				v.Set("password", "secret")
				v.Set("connection", "993")
				return v
			}(),
			expected: "Name is required",
		},
		{
			name: "missing host",
			form: func() url.Values {
				v := url.Values{}
				v.Set("name", "Test")
				v.Set("host", "")
				v.Set("port", "993")
				v.Set("username", "test@test.com")
				v.Set("password", "secret")
				v.Set("connection", "993")
				return v
			}(),
			expected: "Host is required",
		},
		{
			name: "missing username",
			form: func() url.Values {
				v := url.Values{}
				v.Set("name", "Test")
				v.Set("host", "imap.test.com")
				v.Set("port", "993")
				v.Set("username", "")
				v.Set("password", "secret")
				v.Set("connection", "993")
				return v
			}(),
			expected: "Username is required",
		},
		{
			name: "missing password",
			form: func() url.Values {
				v := url.Values{}
				v.Set("name", "Test")
				v.Set("host", "imap.test.com")
				v.Set("port", "993")
				v.Set("username", "test@test.com")
				v.Set("password", "")
				v.Set("connection", "993")
				return v
			}(),
			expected: "Password is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/accounts/new", strings.NewReader(tt.form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			handler.Create(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("expected status 200 (form re-rendered), got %d", w.Code)
			}

			body := w.Body.String()
			if !strings.Contains(body, tt.expected) {
				t.Errorf("expected response to contain %q, got body:\n%s", tt.expected, body)
			}
		})
	}
}

func TestEditForm(t *testing.T) {
	handler, cleanup := newTestHandler(t)
	defer cleanup()

	ctx := context.Background()
	created, err := handler.store.CreateAccount(ctx, "Edit Test", "imap.edit.com", 993, "edit@test.com", "encrypted-pass", true, []string{"INBOX"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/accounts/%d/edit", created.ID), nil)
	w := httptest.NewRecorder()
	handler.EditForm(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "Edit Account") {
		t.Error("expected response to contain 'Edit Account'")
	}
	if !strings.Contains(body, "Edit Test") {
		t.Error("expected response to contain 'Edit Test'")
	}
}

func TestEditFormNotFound(t *testing.T) {
	handler, cleanup := newTestHandler(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/accounts/9999/edit", nil)
	w := httptest.NewRecorder()
	handler.EditForm(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", w.Code)
	}
}

func TestUpdateAccount(t *testing.T) {
	handler, cleanup := newTestHandler(t)
	defer cleanup()

	ctx := context.Background()
	created, err := handler.store.CreateAccount(ctx, "Old Name", "imap.old.com", 993, "old@test.com", "old-encrypted", true, []string{"INBOX"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	form := url.Values{}
	form.Set("name", "New Name")
	form.Set("host", "imap.new.com")
	form.Set("port", "993")
	form.Set("username", "new@test.com")
	form.Set("password", "new-secret")
	form.Set("connection", "993")
	form.Set("folders", "INBOX, Archive")

	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/accounts/%d/edit", created.ID), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.Update(w, req)

	if w.Code != http.StatusSeeOther {
		t.Errorf("expected status 303, got %d", w.Code)
	}

	loc := w.Header().Get("Location")
	if loc != "/accounts" {
		t.Errorf("expected redirect to /accounts, got %q", loc)
	}

	updated, err := handler.store.GetAccount(ctx, created.ID)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if updated.Name != "New Name" {
		t.Errorf("expected name 'New Name', got %q", updated.Name)
	}
	if updated.Host != "imap.new.com" {
		t.Errorf("expected host 'imap.new.com', got %q", updated.Host)
	}
}

func TestUpdateAccountValidationErrors(t *testing.T) {
	handler, cleanup := newTestHandler(t)
	defer cleanup()

	ctx := context.Background()
	created, err := handler.store.CreateAccount(ctx, "Test", "imap.test.com", 993, "test@test.com", "encrypted-pass", true, []string{"INBOX"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	form := url.Values{}
	form.Set("name", "")
	form.Set("host", "imap.test.com")
	form.Set("port", "993")
	form.Set("username", "test@test.com")
	form.Set("password", "newpass")
	form.Set("connection", "993")
	form.Set("folders", "INBOX")

	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/accounts/%d/edit", created.ID), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.Update(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200 (form re-rendered), got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "Name is required") {
		t.Error("expected response to contain validation error")
	}
}

func TestDeleteAccount(t *testing.T) {
	handler, cleanup := newTestHandler(t)
	defer cleanup()

	ctx := context.Background()
	created, err := handler.store.CreateAccount(ctx, "Delete Me", "imap.delete.com", 993, "del@test.com", "encrypted-pass", true, []string{"INBOX"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/accounts/%d/delete", created.ID), nil)
	w := httptest.NewRecorder()
	handler.Delete(w, req)

	if w.Code != http.StatusSeeOther {
		t.Errorf("expected status 303, got %d", w.Code)
	}

	loc := w.Header().Get("Location")
	if loc != "/accounts" {
		t.Errorf("expected redirect to /accounts, got %q", loc)
	}

	_, err = handler.store.GetAccount(ctx, created.ID)
	if err == nil {
		t.Error("expected error after delete")
	}
}

func TestDeleteAccountNotFound(t *testing.T) {
	handler, cleanup := newTestHandler(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodPost, "/accounts/9999/delete", nil)
	w := httptest.NewRecorder()
	handler.Delete(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", w.Code)
	}
}

func TestDeleteAccountWrongMethod(t *testing.T) {
	handler, cleanup := newTestHandler(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/accounts/1/delete", nil)
	w := httptest.NewRecorder()
	handler.Delete(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status 405, got %d", w.Code)
	}
}

func TestRouteAccount(t *testing.T) {
	handler, cleanup := newTestHandler(t)
	defer cleanup()

	ctx := context.Background()
	created, err := handler.store.CreateAccount(ctx, "Route Test", "imap.route.com", 993, "route@test.com", "encrypted-pass", true, []string{"INBOX"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	tests := []struct {
		name     string
		method   string
		path     string
		expected int
		contains string
	}{
		{
			name:     "list accounts",
			method:   http.MethodGet,
			path:     "/accounts",
			expected: http.StatusOK,
			contains: "Route Test",
		},
		{
			name:     "new form",
			method:   http.MethodGet,
			path:     "/accounts/new",
			expected: http.StatusOK,
			contains: "Add Account",
		},
		{
			name:     "edit form",
			method:   http.MethodGet,
			path:     fmt.Sprintf("/accounts/%d/edit", created.ID),
			expected: http.StatusOK,
			contains: "Edit Account",
		},
		{
			name:     "edit not found",
			method:   http.MethodGet,
			path:     "/accounts/9999/edit",
			expected: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != tt.expected {
				t.Errorf("expected status %d, got %d for %s %s", tt.expected, w.Code, tt.method, tt.path)
			}

			if tt.contains != "" {
				body := w.Body.String()
				if !strings.Contains(body, tt.contains) {
					t.Errorf("expected response to contain %q, got body:\n%s", tt.contains, body)
				}
			}
		})
	}
}

func TestCreateAccountWithSTARTTLS(t *testing.T) {
	handler, cleanup := newTestHandler(t)
	defer cleanup()

	form := url.Values{}
	form.Set("name", "STARTTLS Account")
	form.Set("host", "mail.company.com")
	form.Set("port", "143")
	form.Set("username", "user@company.com")
	form.Set("password", "secret")
	form.Set("connection", "143")
	form.Set("folders", "INBOX, Sent, Trash")

	req := httptest.NewRequest(http.MethodPost, "/accounts/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.Create(w, req)

	if w.Code != http.StatusSeeOther {
		t.Errorf("expected status 303, got %d", w.Code)
	}

	ctx := context.Background()
	accts, err := handler.store.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}

	found := false
	for _, a := range accts {
		if a.Name == "STARTTLS Account" {
			found = true
			if a.Port != 143 {
				t.Errorf("expected port 143, got %d", a.Port)
			}
			if a.UseSsl {
				t.Error("expected UseSsl to be false for STARTTLS")
			}
			if len(a.Folders) != 3 {
				t.Errorf("expected 3 folders, got %d", len(a.Folders))
			}
		}
	}
	if !found {
		t.Error("expected to find 'STARTTLS Account' in database")
	}
}

func TestUpdateAccountPreservesUnchanged(t *testing.T) {
	handler, cleanup := newTestHandler(t)
	defer cleanup()

	ctx := context.Background()
	created, err := handler.store.CreateAccount(ctx, "Preserve Test", "imap.preserve.com", 993, "preserve@test.com", "old-encrypted", true, []string{"INBOX", "Archive"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	form := url.Values{}
	form.Set("name", "Preserve Test Updated")
	form.Set("host", "imap.preserve.com")
	form.Set("port", "993")
	form.Set("username", "preserve@test.com")
	form.Set("password", "new-encrypted")
	form.Set("connection", "993")
	form.Set("folders", "INBOX, Archive")

	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/accounts/%d/edit", created.ID), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.Update(w, req)

	if w.Code != http.StatusSeeOther {
		t.Errorf("expected status 303, got %d", w.Code)
	}

	updated, err := handler.store.GetAccount(ctx, created.ID)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if updated.Name != "Preserve Test Updated" {
		t.Errorf("expected updated name, got %q", updated.Name)
	}
	if updated.Host != "imap.preserve.com" {
		t.Errorf("expected preserved host, got %q", updated.Host)
	}
}

// TestUpdateAccountHostChangeNeedsPassword: repointing an account at another
// server without re-entering the password is refused — otherwise UI access
// alone would let someone have katchup send the stored password to a host
// they control.
func TestUpdateAccountHostChangeNeedsPassword(t *testing.T) {
	handler, cleanup := newTestHandler(t)
	defer cleanup()

	ctx := context.Background()
	created, err := handler.store.CreateAccount(ctx, "Mail", "imap.real.com", 993, "me@real.com", "stored", true, []string{"INBOX"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	update := func(password string) *httptest.ResponseRecorder {
		form := url.Values{}
		form.Set("name", "Mail")
		form.Set("host", "imap.attacker.example")
		form.Set("port", "993")
		form.Set("username", "me@real.com")
		form.Set("password", password)
		form.Set("folders", "INBOX")
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/accounts/%d/edit", created.ID), strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		handler.Update(w, req)
		return w
	}

	if w := update(""); w.Code == http.StatusSeeOther {
		t.Fatal("host change without a password was accepted")
	}
	if got, _ := handler.store.GetAccount(ctx, created.ID); got.Host != "imap.real.com" {
		t.Fatalf("host changed to %q without a password", got.Host)
	}

	if w := update("new-password"); w.Code != http.StatusSeeOther {
		t.Fatalf("host change with a password: got %d, want 303", w.Code)
	}
}
