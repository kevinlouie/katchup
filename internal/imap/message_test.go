package imap

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"katchup/internal/account"
	migrations "katchup/sql/migrations"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// migrateTestDB applies the real (embedded) migrations so tests exercise the
// production schema rather than a hand-maintained copy.
func migrateTestDB(t *testing.T, db *sql.DB) {
	t.Helper()
	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.Up(db, "."); err != nil {
		t.Fatalf("migrate: %v", err)
	}
}

func newTestStore(t *testing.T) (*Store, int64) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", dbPath+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	migrateTestDB(t, db)

	acctStore, err := account.New(db, "")
	if err != nil {
		t.Fatalf("account store: %v", err)
	}
	store, err := NewStore(db, acctStore)
	if err != nil {
		t.Fatalf("imap store: %v", err)
	}

	acct, err := acctStore.CreateAccount(context.Background(), "Test", "imap.test.com", 993, "test@test.com", "pass", true, []string{"INBOX"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	return store, acct.ID
}

func TestFuzzyFingerprintDeterministic(t *testing.T) {
	a := fuzzyFingerprint("Sender@Example.com", "2026-07-01T12:00:00Z", "Hello World")
	b := fuzzyFingerprint("sender@example.com", "2026-07-01T12:00:00Z", "hello world")
	if a != b {
		t.Errorf("fuzzy fingerprint not case-insensitive: %s != %s", a, b)
	}

	c := fuzzyFingerprint("other@example.com", "2026-07-01T12:00:00Z", "hello world")
	if a == c {
		t.Errorf("different from should produce different fingerprint")
	}

	if len(a) != 64 {
		t.Errorf("expected 64 hex chars, got %d", len(a))
	}
}

// TestDedupBlobRefcount verifies that the same content stored under two
// different (folder, uid) pairs reuses a single blob whose refcount is bumped,
// while producing two distinct messages rows.
func TestDedupBlobRefcount(t *testing.T) {
	store, accountID := newTestStore(t)
	ctx := context.Background()

	sha := "deadbeef"
	relPath := "1/INBOX/2026-07-01_1.eml"

	// First store: blob does not exist -> insert.
	if _, found, err := store.GetBlobBySha(ctx, accountID, sha); err != nil || found {
		t.Fatalf("expected no blob, got found=%v err=%v", found, err)
	}
	blobID1, err := store.UpsertBlob(ctx, accountID, sha, relPath, 100)
	if err != nil {
		t.Fatalf("upsert blob 1: %v", err)
	}
	if err := store.InsertMessage(ctx, InsertMessageParams{
		AccountID: accountID, Folder: "INBOX", UID: 1, BlobID: blobID1,
		InternalDate: "2026-07-01T12:00:00Z", Size: 100,
	}); err != nil {
		t.Fatalf("insert message 1: %v", err)
	}

	// Second store: identical content under a different folder/uid.
	blob, found, err := store.GetBlobBySha(ctx, accountID, sha)
	if err != nil || !found {
		t.Fatalf("expected existing blob, got found=%v err=%v", found, err)
	}
	blobID2, err := store.UpsertBlob(ctx, accountID, sha, blob.Path, blob.Size)
	if err != nil {
		t.Fatalf("upsert blob 2: %v", err)
	}
	if blobID2 != blobID1 {
		t.Errorf("expected same blob id, got %d and %d", blobID1, blobID2)
	}
	if err := store.InsertMessage(ctx, InsertMessageParams{
		AccountID: accountID, Folder: "Archive", UID: 2, BlobID: blobID2,
		InternalDate: "2026-07-01T12:00:00Z", Size: 100,
	}); err != nil {
		t.Fatalf("insert message 2: %v", err)
	}

	// One blob, refcount 2.
	final, _, err := store.GetBlobBySha(ctx, accountID, sha)
	if err != nil {
		t.Fatalf("get blob: %v", err)
	}
	if final.Refcount != 2 {
		t.Errorf("expected refcount 2, got %d", final.Refcount)
	}

	var blobCount int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM blobs WHERE account_id = ?", accountID).Scan(&blobCount); err != nil {
		t.Fatalf("count blobs: %v", err)
	}
	if blobCount != 1 {
		t.Errorf("expected 1 blob, got %d", blobCount)
	}

	// Two messages rows, one per folder/uid.
	total, err := store.CountMessages(ctx, MessageFilter{AccountID: accountID})
	if err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if total != 2 {
		t.Errorf("expected 2 messages, got %d", total)
	}
}

// TestMessageExistsIdempotent verifies the sync path's idempotency guard.
func TestMessageExistsIdempotent(t *testing.T) {
	store, accountID := newTestStore(t)
	ctx := context.Background()

	blobID, err := store.UpsertBlob(ctx, accountID, "abc", "1/INBOX/x.eml", 10)
	if err != nil {
		t.Fatalf("upsert blob: %v", err)
	}
	if err := store.InsertMessage(ctx, InsertMessageParams{
		AccountID: accountID, Folder: "INBOX", UID: 5, BlobID: blobID,
		InternalDate: "2026-07-01T00:00:00Z", Size: 10,
	}); err != nil {
		t.Fatalf("insert message: %v", err)
	}

	exists, err := store.MessageExists(ctx, accountID, "INBOX", 0, 5)
	if err != nil || !exists {
		t.Fatalf("expected message to exist, got exists=%v err=%v", exists, err)
	}

	missing, err := store.MessageExists(ctx, accountID, "INBOX", 0, 6)
	if err != nil || missing {
		t.Fatalf("expected no message, got exists=%v err=%v", missing, err)
	}
}

// TestLookupArchivedBracketForms: IMAP sync stores the envelope Message-ID with
// angle brackets, backfill stores it bare; either query form must find either.
func TestLookupArchivedBracketForms(t *testing.T) {
	store, accountID := newTestStore(t)
	ctx := context.Background()

	for uid, stored := range map[int64]string{1: "<synced@example.com>", 2: "backfilled@example.com"} {
		blobID, err := store.UpsertBlob(ctx, accountID, stored, stored, 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.InsertMessage(ctx, InsertMessageParams{
			AccountID: accountID, Folder: "INBOX", UID: uid, BlobID: blobID, MessageIDHdr: stored,
		}); err != nil {
			t.Fatal(err)
		}
	}

	for _, q := range []string{
		"<synced@example.com>", "synced@example.com", " <synced@example.com> ", "< synced@example.com >",
		"<backfilled@example.com>", "backfilled@example.com",
	} {
		if _, found, err := store.LookupArchived(ctx, q, ""); err != nil || !found {
			t.Errorf("LookupArchived(%q) found=%v err=%v", q, found, err)
		}
	}
	if _, found, _ := store.LookupArchived(ctx, "<>", ""); found {
		t.Error(`LookupArchived("<>") matched`)
	}
}
