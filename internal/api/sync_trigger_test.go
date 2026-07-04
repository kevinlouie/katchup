package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"katchup/internal/account"
	"katchup/internal/imap"

	_ "modernc.org/sqlite"
)

func newTestSyncTriggerHandler(t *testing.T) (*SyncTriggerHandler, *account.Store, int64) {
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

	store, err := account.New(db, "test-master-key-for-trigger-tests")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	imapStore, err := imap.NewStore(db, store)
	if err != nil {
		t.Fatalf("new imap store: %v", err)
	}
	syncer := imap.NewSyncer(imapStore, t.TempDir(), nil, store)

	acct, err := store.CreateAccount(context.Background(), "Trig", "imap.trig.com", 993, "trig@test.com", "enc", true, []string{"INBOX"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	return NewSyncTriggerHandler(store, syncer, 30*time.Second), store, acct.ID
}

func TestSyncTriggerMissingAccount(t *testing.T) {
	h, _, _ := newTestSyncTriggerHandler(t)

	req := httptest.NewRequest(http.MethodPost, "/api/sync", nil)
	w := httptest.NewRecorder()
	h.Trigger(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestSyncTriggerAccountNotFound(t *testing.T) {
	h, _, _ := newTestSyncTriggerHandler(t)

	req := httptest.NewRequest(http.MethodPost, "/api/sync?account=9999", nil)
	w := httptest.NewRecorder()
	h.Trigger(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// TestSyncTriggerCoalescesOntoRunning verifies the handler returns 202 with the
// existing run id (started=false) when a run is already in flight, and does NOT
// start a background sync.
func TestSyncTriggerCoalescesOntoRunning(t *testing.T) {
	h, store, accountID := newTestSyncTriggerHandler(t)
	ctx := context.Background()

	// Seed a fresh "running" run so TriggerSync coalesces onto it instead of
	// launching a real IMAP sync.
	running, err := store.CreateSyncRun(ctx, accountID)
	if err != nil {
		t.Fatalf("create sync run: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/sync?account=%d", accountID), nil)
	w := httptest.NewRecorder()
	h.Trigger(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d (body: %s)", w.Code, w.Body.String())
	}

	var resp syncTriggerResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Started {
		t.Error("expected started=false when coalescing onto a running run")
	}
	if resp.RunID != running.ID {
		t.Errorf("expected run id %d, got %d", running.ID, resp.RunID)
	}
	if resp.AccountID != accountID {
		t.Errorf("expected account id %d, got %d", accountID, resp.AccountID)
	}

	// No new run row should have been created.
	runs, err := store.ListRecentRuns(ctx, accountID)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run row, got %d", len(runs))
	}
}
