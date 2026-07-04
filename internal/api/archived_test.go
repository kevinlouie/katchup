package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"katchup/internal/account"
	"katchup/internal/imap"

	_ "modernc.org/sqlite"
)

// newTestArchivedHandler builds an ArchivedHandler backed by a temp SQLite DB
// with the message index tables, plus one account to attach messages to.
func newTestArchivedHandler(t *testing.T) (*ArchivedHandler, *imap.Store, int64) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatalf("enable WAL: %v", err)
	}
	if err := createTestTables(db); err != nil {
		t.Fatalf("create tables: %v", err)
	}

	store, err := account.New(db, "test-master-key-for-archived-tests")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	imapStore, err := imap.NewStore(db, store)
	if err != nil {
		t.Fatalf("new imap store: %v", err)
	}

	acct, err := store.CreateAccount(context.Background(), "Archived Test", "imap.arch.com", 993, "arch@test.com", "encrypted-pass", true, []string{"INBOX"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	return NewArchivedHandler(imapStore), imapStore, acct.ID
}

// seedArchived inserts a blob + message row (mirrors the sync path) and returns
// the sha256 used for the blob.
func seedArchived(t *testing.T, s *imap.Store, accountID, uid int64, messageID, fuzzyFP string) string {
	t.Helper()
	ctx := context.Background()

	sha := fmt.Sprintf("sha256-%d-%d", accountID, uid)
	relPath := fmt.Sprintf("%d/INBOX/2026-07-01_%d.eml.enc", accountID, uid)
	blobID, err := s.UpsertBlob(ctx, accountID, sha, relPath, 100)
	if err != nil {
		t.Fatalf("upsert blob: %v", err)
	}
	if err := s.InsertMessage(ctx, imap.InsertMessageParams{
		AccountID:    accountID,
		Folder:       "INBOX",
		UID:          uid,
		BlobID:       blobID,
		MessageIDHdr: messageID,
		FuzzyFP:      fuzzyFP,
		FromAddr:     "sender@example.com",
		Subject:      "Hello",
		InternalDate: "2026-07-01T12:00:00Z",
		Size:         100,
	}); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	return sha
}

func decodeStatus(t *testing.T, w *httptest.ResponseRecorder) ArchivedStatus {
	t.Helper()
	var s ArchivedStatus
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatalf("decode status: %v (body: %s)", err, w.Body.String())
	}
	return s
}

func TestArchived_HitByMessageID(t *testing.T) {
	h, s, acctID := newTestArchivedHandler(t)
	sha := seedArchived(t, s, acctID, 1, "<abc@example.com>", "fuzzy-1")

	req := httptest.NewRequest(http.MethodGet, "/api/archived?message_id=%3Cabc@example.com%3E", nil)
	w := httptest.NewRecorder()
	h.Get(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	got := decodeStatus(t, w)
	if !got.Archived {
		t.Fatalf("expected archived=true, got %+v", got)
	}
	if got.Sha256 != sha {
		t.Errorf("expected sha256 %q, got %q", sha, got.Sha256)
	}
	if got.ID == 0 {
		t.Error("expected non-zero message id")
	}
	if got.ArchivedAt == "" {
		t.Error("expected archived_at to be set")
	}
}

func TestArchived_MissByMessageID(t *testing.T) {
	h, s, acctID := newTestArchivedHandler(t)
	seedArchived(t, s, acctID, 1, "<abc@example.com>", "fuzzy-1")

	req := httptest.NewRequest(http.MethodGet, "/api/archived?message_id=%3Cnope@example.com%3E", nil)
	w := httptest.NewRecorder()
	h.Get(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	got := decodeStatus(t, w)
	if got.Archived {
		t.Fatalf("expected archived=false, got %+v", got)
	}
	if got.ID != 0 || got.Sha256 != "" {
		t.Errorf("expected empty fields on miss, got %+v", got)
	}
}

func TestArchived_FuzzyFallback(t *testing.T) {
	h, s, acctID := newTestArchivedHandler(t)
	seedArchived(t, s, acctID, 1, "<abc@example.com>", "fuzzy-xyz")

	// Message-ID misses, but fp matches -> should fall back and hit.
	req := httptest.NewRequest(http.MethodGet, "/api/archived?message_id=%3Cmiss@example.com%3E&fp=fuzzy-xyz", nil)
	w := httptest.NewRecorder()
	h.Get(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	got := decodeStatus(t, w)
	if !got.Archived {
		t.Fatalf("expected archived=true via fuzzy fallback, got %+v", got)
	}
}

func TestArchived_MissingParams(t *testing.T) {
	h, _, _ := newTestArchivedHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/api/archived", nil)
	w := httptest.NewRecorder()
	h.Get(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestArchived_BatchLookup(t *testing.T) {
	h, s, acctID := newTestArchivedHandler(t)
	seedArchived(t, s, acctID, 1, "<a@example.com>", "f1")
	seedArchived(t, s, acctID, 2, "<b@example.com>", "f2")

	body := `{"message_ids":["<a@example.com>","<b@example.com>","<missing@example.com>"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/archived/lookup", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.Lookup(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}

	var out map[string]ArchivedStatus
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode map: %v (body: %s)", err, w.Body.String())
	}
	if len(out) != 3 {
		t.Fatalf("expected 3 entries, got %d: %+v", len(out), out)
	}
	if !out["<a@example.com>"].Archived {
		t.Error("expected <a@example.com> archived")
	}
	if !out["<b@example.com>"].Archived {
		t.Error("expected <b@example.com> archived")
	}
	if out["<missing@example.com>"].Archived {
		t.Error("expected <missing@example.com> not archived")
	}
}

// --- Auth middleware tests ---

func TestAPIAuth_503WhenTokenUnset(t *testing.T) {
	handler := APIAuth("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/archived?message_id=x", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when token unset, got %d", w.Code)
	}
}

func TestAPIAuth_401WithWrongOrAbsentToken(t *testing.T) {
	reached := false
	handler := APIAuth("secret", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	cases := []struct {
		name   string
		build  func() *http.Request
		wantOK bool
	}{
		{"no token", func() *http.Request {
			return httptest.NewRequest(http.MethodGet, "/api/archived?message_id=x", nil)
		}, false},
		{"wrong bearer", func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/api/archived?message_id=x", nil)
			r.Header.Set("Authorization", "Bearer wrong")
			return r
		}, false},
		{"wrong query", func() *http.Request {
			return httptest.NewRequest(http.MethodGet, "/api/archived?message_id=x&token=wrong", nil)
		}, false},
		{"correct bearer", func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/api/archived?message_id=x", nil)
			r.Header.Set("Authorization", "Bearer secret")
			return r
		}, true},
		{"correct query", func() *http.Request {
			return httptest.NewRequest(http.MethodGet, "/api/archived?message_id=x&token=secret", nil)
		}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reached = false
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, tc.build())

			if tc.wantOK {
				if w.Code != http.StatusOK || !reached {
					t.Fatalf("expected authorized 200, got %d (reached=%v)", w.Code, reached)
				}
			} else {
				if w.Code != http.StatusUnauthorized {
					t.Fatalf("expected 401, got %d", w.Code)
				}
				if reached {
					t.Fatal("inner handler should not be reached on auth failure")
				}
			}
		})
	}
}
