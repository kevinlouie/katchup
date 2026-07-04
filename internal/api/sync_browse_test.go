package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"katchup/internal/account"

	"katchup/internal/imap"

	_ "modernc.org/sqlite"
)

func newTestSyncHandler(t *testing.T) (*SyncHandler, func()) {
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

	store, err := account.New(db, "test-master-key-for-sync-tests")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	imapStore, err := imap.NewStore(db, store)
	if err != nil {
		t.Fatalf("new imap store: %v", err)
	}

	syncer := imap.NewSyncer(imapStore, dir, nil, store)
	handler := NewSyncHandler(store, syncer)

	return handler, func() {
		db.Close()
		os.RemoveAll(dir)
	}
}

func TestSyncStatus(t *testing.T) {
	handler, cleanup := newTestSyncHandler(t)
	defer cleanup()

	ctx := context.Background()
	_, err := handler.store.CreateAccount(ctx, "Sync Test", "imap.sync.com", 993, "sync@test.com", "encrypted-pass", true, []string{"INBOX"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/sync", nil)
	w := httptest.NewRecorder()
	handler.Status(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "Sync Test") {
		t.Error("expected response to contain 'Sync Test'")
	}
	if !strings.Contains(body, "Dashboard") {
		t.Error("expected response to contain 'Dashboard'")
	}
}

func TestSyncStatusWrongMethod(t *testing.T) {
	handler, cleanup := newTestSyncHandler(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodPost, "/sync", nil)
	w := httptest.NewRecorder()
	handler.Status(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status 405, got %d", w.Code)
	}
}

func TestSyncTrigger(t *testing.T) {
	handler, cleanup := newTestSyncHandler(t)
	defer cleanup()

	ctx := context.Background()
	created, err := handler.store.CreateAccount(ctx, "Trigger Test", "imap.trigger.com", 993, "trigger@test.com", "encrypted-pass", true, []string{"INBOX"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/sync/%d/trigger", created.ID), nil)
	w := httptest.NewRecorder()
	handler.Trigger(w, req)

	if w.Code != http.StatusSeeOther {
		t.Errorf("expected status 303, got %d", w.Code)
	}

	loc := w.Header().Get("Location")
	if loc != "/" {
		t.Errorf("expected redirect to /, got %q", loc)
	}
}

func TestSyncTriggerWrongMethod(t *testing.T) {
	handler, cleanup := newTestSyncHandler(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/sync/1/trigger", nil)
	w := httptest.NewRecorder()
	handler.Trigger(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status 405, got %d", w.Code)
	}
}

func TestSyncTriggerNotFound(t *testing.T) {
	handler, cleanup := newTestSyncHandler(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodPost, "/sync/9999/trigger", nil)
	w := httptest.NewRecorder()
	handler.Trigger(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", w.Code)
	}
}

func TestSyncServeHTTP(t *testing.T) {
	handler, cleanup := newTestSyncHandler(t)
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
			name:     "sync status",
			method:   http.MethodGet,
			path:     "/sync",
			expected: http.StatusOK,
			contains: "Dashboard",
		},
		{
			name:     "trigger sync",
			method:   http.MethodPost,
			path:     fmt.Sprintf("/sync/%d/trigger", created.ID),
			expected: http.StatusSeeOther,
		},
		{
			name:     "not found",
			method:   http.MethodGet,
			path:     "/sync/invalid",
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

// Browse handler tests

func newTestBrowseHandler(t *testing.T) (*BrowseHandler, func()) {
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

	store, err := account.New(db, "test-master-key-for-browse-tests")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	imapStore, err := imap.NewStore(db, store)
	if err != nil {
		t.Fatalf("new imap store: %v", err)
	}

	handler := NewBrowseHandler(store, imapStore, dir, nil)

	return handler, func() {
		db.Close()
		os.RemoveAll(dir)
	}
}

// seedMessage inserts a blob + message row and writes a matching file on disk
// under handler.dataDir, mirroring what the sync path produces.
func seedMessage(t *testing.T, h *BrowseHandler, accountID int64, folder, date string, uid int64, from, subject, body string) int64 {
	t.Helper()
	ctx := context.Background()

	relPath := filepath.Join(fmt.Sprintf("%d", accountID), folder, fmt.Sprintf("%s_%d.eml", date, uid))
	absPath := filepath.Join(h.dataDir, relPath)
	if err := os.MkdirAll(filepath.Dir(absPath), 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(absPath, []byte(body), 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	sha := fmt.Sprintf("sha-%d-%d", accountID, uid)
	blobID, err := h.imapStore.UpsertBlob(ctx, accountID, sha, relPath, int64(len(body)))
	if err != nil {
		t.Fatalf("upsert blob: %v", err)
	}
	if err := h.imapStore.InsertMessage(ctx, imap.InsertMessageParams{
		AccountID:    accountID,
		Folder:       folder,
		UID:          uid,
		BlobID:       blobID,
		MessageIDHdr: fmt.Sprintf("<%d@test>", uid),
		FromAddr:     from,
		Subject:      subject,
		InternalDate: date + "T12:00:00Z",
		Size:         int64(len(body)),
	}); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	return blobID
}

func TestBrowseList(t *testing.T) {
	handler, cleanup := newTestBrowseHandler(t)
	defer cleanup()

	ctx := context.Background()
	_, err := handler.store.CreateAccount(ctx, "Browse Test", "imap.browse.com", 993, "browse@test.com", "encrypted-pass", true, []string{"INBOX"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/browse", nil)
	w := httptest.NewRecorder()
	handler.List(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "Browse Test") {
		t.Error("expected response to contain 'Browse Test'")
	}
	if !strings.Contains(body, "Browse") {
		t.Error("expected response to contain 'Browse'")
	}
}

func TestBrowseListWrongMethod(t *testing.T) {
	handler, cleanup := newTestBrowseHandler(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodPost, "/browse", nil)
	w := httptest.NewRecorder()
	handler.List(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status 405, got %d", w.Code)
	}
}

func TestBrowseListWithFilter(t *testing.T) {
	handler, cleanup := newTestBrowseHandler(t)
	defer cleanup()

	ctx := context.Background()
	created, err := handler.store.CreateAccount(ctx, "Filtered Browse", "imap.filter.com", 993, "filter@test.com", "encrypted-pass", true, []string{"INBOX"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	for i := 0; i < 3; i++ {
		seedMessage(t, handler, created.ID, "INBOX", "2026-07-01", int64(10000+i),
			"sender@example.com", fmt.Sprintf("Subject %d", i), "test email content")
	}

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/browse?account=%d", created.ID), nil)
	w := httptest.NewRecorder()
	handler.List(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "2026-07-01") {
		t.Error("expected response to contain date '2026-07-01'")
	}
	if !strings.Contains(body, "sender@example.com") {
		t.Error("expected response to contain sender address")
	}
}

func TestBrowseDownload(t *testing.T) {
	handler, cleanup := newTestBrowseHandler(t)
	defer cleanup()

	ctx := context.Background()
	created, err := handler.store.CreateAccount(ctx, "DL", "imap.dl.com", 993, "dl@test.com", "encrypted-pass", true, []string{"INBOX"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	testEmail := "From: test@example.com\nSubject: Test\n\nThis is a test email."
	seedMessage(t, handler, created.ID, "INBOX", "2026-07-01", 12345,
		"test@example.com", "Test", testEmail)

	// message id is 1 (first inserted row).
	req := httptest.NewRequest(http.MethodGet, "/browse/download/1", nil)
	req.SetPathValue("id", "1")
	w := httptest.NewRecorder()
	handler.Download(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (body: %s)", w.Code, w.Body.String())
	}

	if w.Header().Get("Content-Type") != "message/rfc822" {
		t.Errorf("expected Content-Type 'message/rfc822', got %q", w.Header().Get("Content-Type"))
	}

	body := w.Body.String()
	if body != testEmail {
		t.Errorf("expected response to contain test email content, got %q", body)
	}
}

func TestBrowseDownloadNotFound(t *testing.T) {
	handler, cleanup := newTestBrowseHandler(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/browse/download/9999", nil)
	req.SetPathValue("id", "9999")
	w := httptest.NewRecorder()
	handler.Download(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", w.Code)
	}
}

func TestBrowseDownloadInvalidID(t *testing.T) {
	handler, cleanup := newTestBrowseHandler(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/browse/download/not-a-number", nil)
	req.SetPathValue("id", "not-a-number")
	w := httptest.NewRecorder()
	handler.Download(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", w.Code)
	}
}

func TestBrowseServeHTTP(t *testing.T) {
	handler, cleanup := newTestBrowseHandler(t)
	defer cleanup()

	tests := []struct {
		name     string
		method   string
		path     string
		expected int
		contains string
	}{
		{
			name:     "browse list",
			method:   http.MethodGet,
			path:     "/browse",
			expected: http.StatusOK,
			contains: "Browse",
		},
		{
			name:     "not found",
			method:   http.MethodGet,
			path:     "/browse/invalid",
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

func TestParseEMLFilename(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		date     string
		uid      string
		ok       bool
	}{
		{
			name:     "valid eml",
			filename: "2026-07-01_12345.eml",
			date:     "2026-07-01",
			uid:      "12345",
			ok:       true,
		},
		{
			name:     "valid encrypted eml",
			filename: "2026-07-01_12345.eml.enc",
			date:     "2026-07-01",
			uid:      "12345",
			ok:       true,
		},
		{
			name:     "invalid format",
			filename: "invalid.eml",
			ok:       false,
		},
		{
			name:     "invalid date format",
			filename: "20260701_12345.eml",
			ok:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			date, uid, ok := parseEMLFilename(tt.filename)
			if ok != tt.ok {
				t.Errorf("expected ok=%v, got %v", tt.ok, ok)
			}
			if ok {
				if date != tt.date {
					t.Errorf("expected date %q, got %q", tt.date, date)
				}
				if uid != tt.uid {
					t.Errorf("expected uid %q, got %q", tt.uid, uid)
				}
			}
		})
	}
}

func formatSizePublic(bytes int64) string {
	return formatSize(bytes)
}

func TestFormatSize(t *testing.T) {
	tests := []struct {
		size     int64
		expected string
	}{
		{0, "0B"},
		{100, "100B"},
		{1024, "1.0KB"},
		{1536, "1.5KB"},
		{1048576, "1.0MB"},
		{1073741824, "1.0GB"},
	}

	for _, tt := range tests {
		result := formatSizePublic(tt.size)
		if result != tt.expected {
			t.Errorf("formatSize(%d) = %q, expected %q", tt.size, result, tt.expected)
		}
	}
}

func TestBrowseListPagination(t *testing.T) {
	handler, cleanup := newTestBrowseHandler(t)
	defer cleanup()

	ctx := context.Background()
	created, err := handler.store.CreateAccount(ctx, "Pagination Test", "imap.paginate.com", 993, "paginate@test.com", "encrypted-pass", true, []string{"INBOX"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	// Create more than 50 messages
	for i := 0; i < 55; i++ {
		seedMessage(t, handler, created.ID, "INBOX", "2026-07-01", int64(10000+i),
			"sender@example.com", fmt.Sprintf("Subject %d", i), "test email content")
	}

	// Test first page
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/browse?account=%d&page=1", created.ID), nil)
	w := httptest.NewRecorder()
	handler.List(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "Page ") {
		t.Error("expected response to contain pagination info")
	}
}

func TestBrowseListEmpty(t *testing.T) {
	handler, cleanup := newTestBrowseHandler(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/browse", nil)
	w := httptest.NewRecorder()
	handler.List(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "No emails found") {
		t.Error("expected response to contain 'No emails found'")
	}
}

func TestBrowseListAccountFilter(t *testing.T) {
	handler, cleanup := newTestBrowseHandler(t)
	defer cleanup()

	ctx := context.Background()
	created, err := handler.store.CreateAccount(ctx, "Filter Test", "imap.filter.com", 993, "filter@test.com", "encrypted-pass", true, []string{"INBOX"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	for i := 0; i < 3; i++ {
		seedMessage(t, handler, created.ID, "INBOX", "2026-07-01", int64(10000+i),
			"sender@example.com", fmt.Sprintf("Subject %d", i), "test email content")
	}

	// Test with account filter
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/browse?account=%d", created.ID), nil)
	w := httptest.NewRecorder()
	handler.List(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "2026-07-01") {
		t.Error("expected response to contain date '2026-07-01'")
	}
}
