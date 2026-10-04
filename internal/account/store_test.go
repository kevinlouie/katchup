package account

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	migrations "katchup/sql/migrations"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// newTestStore creates a temporary SQLite database, runs migrations, and returns a Store.
func newTestStore(t *testing.T) (*Store, func()) {
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

	if err := createTables(db); err != nil {
		t.Fatalf("create tables: %v", err)
	}

	store, err := New(db, "test-master-key-for-unit-tests")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	return store, func() {
		db.Close()
		os.RemoveAll(dir)
	}
}

// createTables applies the real (embedded) migrations.
func createTables(db *sql.DB) error {
	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		return err
	}
	return goose.Up(db, ".")
}

func TestCreateAccount(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	ctx := context.Background()

	acct, err := store.CreateAccount(ctx, "Personal Gmail", "imap.gmail.com", 993, "user@gmail.com", "encrypted-pass", true, []string{"INBOX", "Archive"})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	if acct.ID == 0 {
		t.Error("expected non-zero ID")
	}
	if acct.Name != "Personal Gmail" {
		t.Errorf("expected name 'Personal Gmail', got %q", acct.Name)
	}
	if acct.Host != "imap.gmail.com" {
		t.Errorf("expected host 'imap.gmail.com', got %q", acct.Host)
	}
	if acct.Port != 993 {
		t.Errorf("expected port 993, got %d", acct.Port)
	}
	if acct.Username != "user@gmail.com" {
		t.Errorf("expected username 'user@gmail.com', got %q", acct.Username)
	}
	if !acct.UseSsl {
		t.Error("expected UseSsl to be true")
	}
	if len(acct.Folders) != 2 || acct.Folders[0] != "INBOX" || acct.Folders[1] != "Archive" {
		t.Errorf("expected folders [INBOX Archive], got %v", acct.Folders)
	}
	if acct.CreatedAt == "" {
		t.Error("expected non-empty created_at")
	}
	if acct.UpdatedAt == "" {
		t.Error("expected non-empty updated_at")
	}
}

func TestGetAccount(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// Create an account first
	created, err := store.CreateAccount(ctx, "Work", "mail.work.com", 993, "user@work.com", "pass123", true, []string{"INBOX"})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	// Get the account
	acct, err := store.GetAccount(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}

	if acct.ID != created.ID {
		t.Errorf("expected ID %d, got %d", created.ID, acct.ID)
	}
	if acct.Name != "Work" {
		t.Errorf("expected name 'Work', got %q", acct.Name)
	}
}

func TestGetAccountNotFound(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	ctx := context.Background()

	_, err := store.GetAccount(ctx, 9999)
	if err == nil {
		t.Fatal("expected error for non-existent account")
	}
}

func TestListAccounts(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// Create multiple accounts
	if _, err := store.CreateAccount(ctx, "Beta", "imap.beta.com", 143, "beta@beta.com", "pass", false, []string{"INBOX"}); err != nil {
		t.Fatalf("CreateAccount 1: %v", err)
	}
	if _, err := store.CreateAccount(ctx, "Alpha", "imap.alpha.com", 993, "alpha@alpha.com", "pass", true, []string{"INBOX", "Spam"}); err != nil {
		t.Fatalf("CreateAccount 2: %v", err)
	}

	// List all accounts
	accounts, err := store.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}

	if len(accounts) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(accounts))
	}

	// Should be sorted by name ASC
	if accounts[0].Name != "Alpha" {
		t.Errorf("expected first account 'Alpha', got %q", accounts[0].Name)
	}
	if accounts[1].Name != "Beta" {
		t.Errorf("expected second account 'Beta', got %q", accounts[1].Name)
	}
}

func TestUpdateAccount(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// Create an account
	created, err := store.CreateAccount(ctx, "Old Name", "old.com", 993, "old@old.com", "oldpass", true, []string{"INBOX"})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	// Update the account
	updated, err := store.UpdateAccount(ctx, created.ID, "New Name", "new.com", 143, "new@new.com", "newpass", false, []string{"INBOX", "Trash"})
	if err != nil {
		t.Fatalf("UpdateAccount: %v", err)
	}

	if updated.Name != "New Name" {
		t.Errorf("expected name 'New Name', got %q", updated.Name)
	}
	if updated.Host != "new.com" {
		t.Errorf("expected host 'new.com', got %q", updated.Host)
	}
	if updated.Port != 143 {
		t.Errorf("expected port 143, got %d", updated.Port)
	}
	if updated.Username != "new@new.com" {
		t.Errorf("expected username 'new@new.com', got %q", updated.Username)
	}
	if updated.UseSsl {
		t.Error("expected UseSsl to be false")
	}
	if len(updated.Folders) != 2 {
		t.Errorf("expected 2 folders, got %d", len(updated.Folders))
	}
	if updated.ID != created.ID {
		t.Errorf("expected ID %d, got %d", created.ID, updated.ID)
	}
}

func TestDeleteAccount(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// Create an account
	created, err := store.CreateAccount(ctx, "To Delete", "delete.com", 993, "del@del.com", "pass", true, []string{"INBOX"})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	// Delete the account
	if err := store.DeleteAccount(ctx, created.ID); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}

	// Verify it's gone
	_, err = store.GetAccount(ctx, created.ID)
	if err == nil {
		t.Fatal("expected error after delete")
	}

	// Verify list is empty
	accounts, err := store.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(accounts) != 0 {
		t.Errorf("expected 0 accounts after delete, got %d", len(accounts))
	}

	// The row survives (so its archived mail keeps its index), with the
	// stored password wiped.
	ids, err := store.ListAllAccountIDs(ctx)
	if err != nil || len(ids) != 1 || ids[0] != created.ID {
		t.Fatalf("ListAllAccountIDs = %v, %v; want [%d]", ids, err, created.ID)
	}
	var pw string
	var deletedAt sql.NullString
	if err := store.db.QueryRow("SELECT encrypted_password, deleted_at FROM accounts WHERE id = ?", created.ID).Scan(&pw, &deletedAt); err != nil {
		t.Fatal(err)
	}
	if pw != "" || !deletedAt.Valid {
		t.Errorf("after delete: password=%q deleted_at=%v; want wiped and set", pw, deletedAt)
	}

	// Updating a deleted account is refused.
	if _, err := store.UpdateAccount(ctx, created.ID, "x", "h", 993, "u", "p", true, nil); err == nil {
		t.Error("expected UpdateAccount on a deleted account to fail")
	}
}

func TestCreateSyncRun(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// Create an account first
	acct, err := store.CreateAccount(ctx, "Test", "imap.test.com", 993, "test@test.com", "pass", true, []string{"INBOX"})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	// Create a sync run
	run, err := store.CreateSyncRun(ctx, acct.ID)
	if err != nil {
		t.Fatalf("CreateSyncRun: %v", err)
	}

	if run.ID == 0 {
		t.Error("expected non-zero sync run ID")
	}
	if run.AccountID != acct.ID {
		t.Errorf("expected account_id %d, got %d", acct.ID, run.AccountID)
	}
	if run.Status != "running" {
		t.Errorf("expected status 'running', got %q", run.Status)
	}
	if run.StartedAt == "" {
		t.Error("expected non-empty started_at")
	}
}

func TestUpdateSyncRunStatus(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// Create an account and sync run
	acct, err := store.CreateAccount(ctx, "Test", "imap.test.com", 993, "test@test.com", "pass", true, []string{"INBOX"})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	run, err := store.CreateSyncRun(ctx, acct.ID)
	if err != nil {
		t.Fatalf("CreateSyncRun: %v", err)
	}

	// Update the sync run status
	uid := int64(42)
	updated, err := store.UpdateSyncRunStatus(ctx, run.ID, 5, "connection timeout", "failed", uid)
	if err != nil {
		t.Fatalf("UpdateSyncRunStatus: %v", err)
	}

	if updated.Status != "failed" {
		t.Errorf("expected status 'failed', got %q", updated.Status)
	}
	if updated.EmailsBackedUp != 5 {
		t.Errorf("expected 5 emails backed up, got %d", updated.EmailsBackedUp)
	}
	if updated.LastUid == nil || *updated.LastUid != 42 {
		t.Errorf("expected last_uid 42, got %v", updated.LastUid)
	}
	if updated.FinishedAt == nil {
		t.Error("expected non-nil finished_at")
	}
}

func TestListRecentRuns(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// Create an account
	acct, err := store.CreateAccount(ctx, "Test", "imap.test.com", 993, "test@test.com", "pass", true, []string{"INBOX"})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	// Create multiple sync runs
	for i := 0; i < 5; i++ {
		run, err := store.CreateSyncRun(ctx, acct.ID)
		if err != nil {
			t.Fatalf("CreateSyncRun %d: %v", i, err)
		}
		uid := int64(10 + i)
		_, err = store.UpdateSyncRunStatus(ctx, run.ID, int64(i*10), "", "completed", uid)
		if err != nil {
			t.Fatalf("UpdateSyncRunStatus %d: %v", i, err)
		}
	}

	// List recent runs
	runs, err := store.ListRecentRuns(ctx, acct.ID)
	if err != nil {
		t.Fatalf("ListRecentRuns: %v", err)
	}

	if len(runs) != 5 {
		t.Fatalf("expected 5 runs, got %d", len(runs))
	}

	// Should be ordered by started_at DESC (most recent first)
	if runs[0].EmailsBackedUp != 40 {
		t.Errorf("expected first run to have 40 emails, got %d", runs[0].EmailsBackedUp)
	}
}

func TestDefaultFolders(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// Create account with empty folders list
	acct, err := store.CreateAccount(ctx, "Default Folders", "imap.test.com", 993, "test@test.com", "pass", true, []string{})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	if len(acct.Folders) != 1 || acct.Folders[0] != "INBOX" {
		t.Errorf("expected default folder [INBOX], got %v", acct.Folders)
	}
}
