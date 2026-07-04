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
	"katchup/internal/search"

	_ "modernc.org/sqlite"
)

func newTestSearchHandler(t *testing.T, searcher search.Searcher) (*SearchHandler, *imap.Store, func()) {
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

	store, err := account.New(db, "test-master-key-for-search-tests")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	imapStore, err := imap.NewStore(db, store)
	if err != nil {
		t.Fatalf("new imap store: %v", err)
	}

	handler := NewSearchHandler(store, imapStore, searcher)
	return handler, imapStore, func() {
		db.Close()
		os.RemoveAll(dir)
	}
}

func seedSearchMessage(t *testing.T, s *imap.Store, accountID, uid int64, from, subject string) {
	t.Helper()
	ctx := context.Background()
	sha := fmt.Sprintf("sha-%d-%d", accountID, uid)
	blobID, err := s.UpsertBlob(ctx, accountID, sha, fmt.Sprintf("%d/INBOX/%d.eml", accountID, uid), 100)
	if err != nil {
		t.Fatalf("upsert blob: %v", err)
	}
	if err := s.InsertMessage(ctx, imap.InsertMessageParams{
		AccountID:    accountID,
		Folder:       "INBOX",
		UID:          uid,
		BlobID:       blobID,
		MessageIDHdr: fmt.Sprintf("<%d@test>", uid),
		FromAddr:     from,
		Subject:      subject,
		InternalDate: "2026-07-01T12:00:00Z",
		Size:         100,
	}); err != nil {
		t.Fatalf("insert message: %v", err)
	}
}

// TestSearchLikeFallbackMatchesSubject verifies that with no Meili searcher, the
// handler serves results from the SQLite LIKE query over subjects.
func TestSearchLikeFallbackMatchesSubject(t *testing.T) {
	handler, imapStore, cleanup := newTestSearchHandler(t, nil)
	defer cleanup()

	ctx := context.Background()
	acct, err := handler.store.CreateAccount(ctx, "Search Test", "imap.s.com", 993, "s@test.com", "enc", true, []string{"INBOX"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	seedSearchMessage(t, imapStore, acct.ID, 1, "alice@example.com", "Invoice for July")
	seedSearchMessage(t, imapStore, acct.ID, 2, "bob@example.com", "Lunch plans")

	req := httptest.NewRequest(http.MethodGet, "/search?q=invoice", nil)
	w := httptest.NewRecorder()
	handler.Search(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Invoice for July") {
		t.Error("expected matching subject 'Invoice for July' in results")
	}
	if strings.Contains(body, "Lunch plans") {
		t.Error("non-matching subject 'Lunch plans' should not appear")
	}
	if !strings.Contains(body, "database") {
		t.Error("expected the DB fallback backend label")
	}
}

// TestSearchLikeFallbackMatchesFrom verifies the LIKE fallback also matches sender.
func TestSearchLikeFallbackMatchesFrom(t *testing.T) {
	handler, imapStore, cleanup := newTestSearchHandler(t, nil)
	defer cleanup()

	ctx := context.Background()
	acct, err := handler.store.CreateAccount(ctx, "Search From", "imap.s.com", 993, "s@test.com", "enc", true, []string{"INBOX"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	seedSearchMessage(t, imapStore, acct.ID, 1, "alice@example.com", "Hello")
	seedSearchMessage(t, imapStore, acct.ID, 2, "bob@example.com", "World")

	req := httptest.NewRequest(http.MethodGet, "/search?q=alice", nil)
	w := httptest.NewRecorder()
	handler.Search(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "alice@example.com") {
		t.Error("expected sender match 'alice@example.com' in results")
	}
	if strings.Contains(body, "bob@example.com") {
		t.Error("non-matching sender should not appear")
	}
}

// TestSearchEmptyQueryShowsPrompt verifies the initial page renders without a query.
func TestSearchEmptyQuery(t *testing.T) {
	handler, _, cleanup := newTestSearchHandler(t, nil)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/search", nil)
	w := httptest.NewRecorder()
	handler.Search(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Search your archive") {
		t.Error("expected the initial search prompt")
	}
}

// stubSearcher returns canned results and records that it was called.
type stubSearcher struct {
	called  bool
	results []search.Result
	err     error
}

func (s *stubSearcher) Search(_ context.Context, _ string, _ int64, _ int) ([]search.Result, error) {
	s.called = true
	return s.results, s.err
}

// TestSearchUsesMeiliWhenConfigured verifies the handler prefers the Meili
// searcher and renders its hits.
func TestSearchUsesMeiliWhenConfigured(t *testing.T) {
	stub := &stubSearcher{results: []search.Result{
		{ID: 5, From: "carol@example.com", Subject: "From Meili", Folder: "INBOX", Date: "2026-07-01T00:00:00Z"},
	}}
	handler, _, cleanup := newTestSearchHandler(t, stub)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/search?q=anything", nil)
	w := httptest.NewRecorder()
	handler.Search(w, req)

	if !stub.called {
		t.Fatal("expected Meili searcher to be queried")
	}
	body := w.Body.String()
	if !strings.Contains(body, "From Meili") {
		t.Error("expected Meili hit subject in results")
	}
	if !strings.Contains(body, "meilisearch") {
		t.Error("expected the meilisearch backend label")
	}
	if !strings.Contains(body, "/browse/download/5") {
		t.Error("expected result to link to /browse/download/5")
	}
}

// TestSearchFallsBackWhenMeiliErrors verifies a Meili error degrades to DB LIKE.
func TestSearchFallsBackWhenMeiliErrors(t *testing.T) {
	stub := &stubSearcher{err: fmt.Errorf("meili down")}
	handler, imapStore, cleanup := newTestSearchHandler(t, stub)
	defer cleanup()

	ctx := context.Background()
	acct, err := handler.store.CreateAccount(ctx, "Fallback", "imap.s.com", 993, "s@test.com", "enc", true, []string{"INBOX"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	seedSearchMessage(t, imapStore, acct.ID, 1, "dave@example.com", "Recoverable subject")

	req := httptest.NewRequest(http.MethodGet, "/search?q=recoverable", nil)
	w := httptest.NewRecorder()
	handler.Search(w, req)

	body := w.Body.String()
	if !stub.called {
		t.Fatal("expected Meili to be attempted first")
	}
	if !strings.Contains(body, "Recoverable subject") {
		t.Error("expected DB fallback to return the matching subject after Meili error")
	}
	if !strings.Contains(body, "database") {
		t.Error("expected the DB backend label after fallback")
	}
}

func TestSearchWrongMethod(t *testing.T) {
	handler, _, cleanup := newTestSearchHandler(t, nil)
	defer cleanup()

	req := httptest.NewRequest(http.MethodPost, "/search", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}
