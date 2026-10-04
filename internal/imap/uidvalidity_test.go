package imap

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	migrations "katchup/sql/migrations"

	"github.com/pressly/goose/v3"
)

func folderState(t *testing.T, s *Syncer, accountID int64, folder string) (lastUID, uidValidity int64) {
	t.Helper()
	lastUID, uidValidity, err := s.store.accountSt.GetFolderSyncState(context.Background(), accountID, folder)
	if err != nil {
		t.Fatalf("get folder state: %v", err)
	}
	return lastUID, uidValidity
}

func insertTestMessage(t *testing.T, store *Store, accountID int64, folder string, uidValidity, uid int64) {
	t.Helper()
	ctx := context.Background()
	blobID, err := store.UpsertBlob(ctx, accountID, folder+"-sha", folder+"/x.eml", 1)
	if err != nil {
		t.Fatalf("upsert blob: %v", err)
	}
	if err := store.InsertMessage(ctx, InsertMessageParams{
		AccountID: accountID, Folder: folder, UIDValidity: uidValidity, UID: uid, BlobID: blobID,
	}); err != nil {
		t.Fatalf("insert message: %v", err)
	}
}

func exists(t *testing.T, store *Store, accountID int64, folder string, uidValidity, uid int64) bool {
	t.Helper()
	ok, err := store.MessageExists(context.Background(), accountID, folder, uidValidity, uid)
	if err != nil {
		t.Fatalf("message exists: %v", err)
	}
	return ok
}

func TestReconcileUIDValidity(t *testing.T) {
	s, accountID, _ := newTestSyncer(t)
	ctx := context.Background()

	// A folder synced before UIDVALIDITY tracking: watermark 50, rows with 0.
	if err := s.store.accountSt.UpsertFolderSyncState(ctx, accountID, "INBOX", 50); err != nil {
		t.Fatal(err)
	}
	insertTestMessage(t, s.store, accountID, "INBOX", 0, 50)

	// First sight of the server value: adopt it, keep the watermark, and stamp
	// the legacy rows so they still count as archived.
	got, err := s.reconcileUIDValidity(ctx, accountID, "INBOX", 100, 0)
	if err != nil || got != 50 {
		t.Fatalf("adopt: got %d, %v; want 50", got, err)
	}
	if last, v := folderState(t, s, accountID, "INBOX"); last != 50 || v != 100 {
		t.Fatalf("adopt: state = (%d, %d), want (50, 100)", last, v)
	}
	if !exists(t, s.store, accountID, "INBOX", 100, 50) {
		t.Fatal("adopt: legacy row not stamped with the adopted UIDVALIDITY")
	}

	// Unchanged: resume from the watermark.
	if got, err := s.reconcileUIDValidity(ctx, accountID, "INBOX", 100, 0); err != nil || got != 50 {
		t.Fatalf("unchanged: got %d, %v; want 50", got, err)
	}

	// Server reports no UIDVALIDITY: leave everything as is.
	if got, err := s.reconcileUIDValidity(ctx, accountID, "INBOX", 0, 0); err != nil || got != 50 {
		t.Fatalf("server 0: got %d, %v; want 50", got, err)
	}
	if _, v := folderState(t, s, accountID, "INBOX"); v != 100 {
		t.Fatalf("server 0 changed stored UIDVALIDITY to %d", v)
	}

	// Renumbered: reset the watermark so new mail below it isn't skipped.
	if got, err := s.reconcileUIDValidity(ctx, accountID, "INBOX", 200, 0); err != nil || got != 0 {
		t.Fatalf("changed: got %d, %v; want 0", got, err)
	}
	if last, v := folderState(t, s, accountID, "INBOX"); last != 0 || v != 200 {
		t.Fatalf("changed: state = (%d, %d), want (0, 200)", last, v)
	}

	// The archived message is kept, and the same UID in the new generation is a
	// different message that must be fetched (not skipped as already indexed).
	if !exists(t, s.store, accountID, "INBOX", 100, 50) {
		t.Fatal("changed: archived message from the old generation was lost")
	}
	if exists(t, s.store, accountID, "INBOX", 200, 50) {
		t.Fatal("changed: new-generation UID 50 wrongly reported as already archived")
	}
	insertTestMessage(t, s.store, accountID, "INBOX", 200, 50)
	if n, _ := s.store.CountMessagesByAccount(ctx, accountID); n != 2 {
		t.Fatalf("expected both generations stored, got %d messages", n)
	}

	// Folders are independent: a fresh folder starts at 0 and records the value.
	if got, err := s.reconcileUIDValidity(ctx, accountID, "Archive", 7, 0); err != nil || got != 0 {
		t.Fatalf("new folder: got %d, %v; want 0", got, err)
	}
	if _, v := folderState(t, s, accountID, "Archive"); v != 7 {
		t.Fatalf("new folder: stored UIDVALIDITY %d, want 7", v)
	}
}

// TestReconcileUIDValidityFirstSightUIDNext: on first sight of a folder's
// UIDVALIDITY, UIDNEXT tells whether the pre-tracking watermark still fits
// the server's numbering.
func TestReconcileUIDValidityFirstSightUIDNext(t *testing.T) {
	tests := []struct {
		name     string
		uidNext  int64
		wantLast int64 // returned and stored watermark
		stamped  bool  // legacy row moved to the server's generation
	}{
		{"consistent: adopt and keep the watermark", 51, 50, true},
		{"UIDNEXT unknown: adopt as before", 0, 50, true},
		{"renumbered before tracking: re-scan, leave old rows", 50, 0, false},
		{"renumbered, UIDNEXT well below the watermark", 4, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, accountID, _ := newTestSyncer(t)
			ctx := context.Background()
			if err := s.store.accountSt.UpsertFolderSyncState(ctx, accountID, "INBOX", 50); err != nil {
				t.Fatal(err)
			}
			insertTestMessage(t, s.store, accountID, "INBOX", 0, 3)

			got, err := s.reconcileUIDValidity(ctx, accountID, "INBOX", 100, tt.uidNext)
			if err != nil || got != tt.wantLast {
				t.Fatalf("got %d, %v; want %d", got, err, tt.wantLast)
			}
			if last, v := folderState(t, s, accountID, "INBOX"); last != tt.wantLast || v != 100 {
				t.Fatalf("state = (%d, %d), want (%d, 100)", last, v, tt.wantLast)
			}
			if stamped := exists(t, s.store, accountID, "INBOX", 100, 3); stamped != tt.stamped {
				t.Fatalf("legacy row stamped = %v, want %v", stamped, tt.stamped)
			}
			if !tt.stamped && !exists(t, s.store, accountID, "INBOX", 0, 3) {
				t.Fatal("legacy row lost")
			}

			// Idempotent: the next run sees the recorded generation.
			if got, err := s.reconcileUIDValidity(ctx, accountID, "INBOX", 100, max(tt.uidNext, 51)); err != nil || got != tt.wantLast {
				t.Fatalf("second call: got %d, %v; want %d", got, err, tt.wantLast)
			}
		})
	}
}

// TestReconcileUIDValiditySameGenerationBadUIDNext: an unchanged UIDVALIDITY
// with UIDNEXT at or below the watermark (a server bug) re-scans this run
// without discarding the stored watermark.
func TestReconcileUIDValiditySameGenerationBadUIDNext(t *testing.T) {
	s, accountID, _ := newTestSyncer(t)
	ctx := context.Background()
	if err := s.store.accountSt.SetFolderSyncState(ctx, accountID, "INBOX", 100, 50); err != nil {
		t.Fatal(err)
	}
	if got, err := s.reconcileUIDValidity(ctx, accountID, "INBOX", 100, 20); err != nil || got != 0 {
		t.Fatalf("got %d, %v; want 0", got, err)
	}
	if last, v := folderState(t, s, accountID, "INBOX"); last != 50 || v != 100 {
		t.Fatalf("state = (%d, %d), want (50, 100) untouched", last, v)
	}
}

// TestSyncFolderRenumberedBeforeTracking: end to end, a folder whose old
// watermark is above the server's UIDNEXT has its new mail archived instead
// of skipped forever.
func TestSyncFolderRenumberedBeforeTracking(t *testing.T) {
	s, accountID, _ := newTestSyncer(t)
	f := newFakeIMAP(t)
	useFakeIMAP(s, f)
	f.setUIDValidity(1700)
	ctx := context.Background()

	if err := s.store.accountSt.UpsertFolderSyncState(ctx, accountID, "INBOX", 500); err != nil {
		t.Fatal(err)
	}
	insertTestMessage(t, s.store, accountID, "INBOX", 0, 2)
	f.add(1, 2, 3)

	if err := s.Run(ctx, accountID); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, uid := range []int64{1, 2, 3} {
		if !exists(t, s.store, accountID, "INBOX", 1700, uid) {
			t.Errorf("UID %d of the renumbered folder not archived", uid)
		}
	}
	if !exists(t, s.store, accountID, "INBOX", 0, 2) {
		t.Error("pre-tracking row lost")
	}
	if last, v := folderState(t, s, accountID, "INBOX"); last != 3 || v != 1700 {
		t.Errorf("state = (%d, %d), want (3, 1700)", last, v)
	}
}

func TestRelEmlPathUIDValidity(t *testing.T) {
	s, _, _ := newTestSyncer(t)
	if got, want := s.relEmlPath(3, "INBOX", "2026-01-02", 0, 9), filepath.Join("3", "INBOX", "2026-01-02_9.eml.enc"); got != want {
		t.Errorf("legacy path = %q, want %q", got, want)
	}
	if got, want := s.relEmlPath(3, "INBOX", "2026-01-02", 1700, 9), filepath.Join("3", "INBOX", "2026-01-02_1700_9.eml.enc"); got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}

func TestParseUIDsFromName(t *testing.T) {
	tests := []struct {
		name        string
		uidValidity int64
		uid         int64
		ok          bool
	}{
		{"2026-01-02_9.eml.enc", 0, 9, true},
		{"2026-01-02_9.eml", 0, 9, true},
		{"2026-01-02_1700_9.eml.enc", 1700, 9, true},
		{"2026-01-02.eml.enc", 0, 0, false},
		{"2026-01-02_x.eml.enc", 0, 0, false},
		{"2026-01-02_x_9.eml.enc", 0, 0, false},
		{"2026-01-02_1_2_3.eml.enc", 0, 0, false},
	}
	for _, tt := range tests {
		v, uid, ok := parseUIDsFromName(tt.name)
		if v != tt.uidValidity || uid != tt.uid || ok != tt.ok {
			t.Errorf("parseUIDsFromName(%q) = (%d, %d, %v), want (%d, %d, %v)",
				tt.name, v, uid, ok, tt.uidValidity, tt.uid, tt.ok)
		}
	}
}

// TestBackfillUIDValidityGenerations: files from several UIDVALIDITY
// generations are all indexed under their own generation, and the watermark
// comes from the newest one only.
func TestBackfillUIDValidityGenerations(t *testing.T) {
	s, accountID, dataDir := newTestSyncer(t)
	ctx := context.Background()

	for _, f := range []struct {
		v   int64
		uid uint32
	}{{0, 900}, {5, 300}, {7, 2}} {
		rel := s.relEmlPath(accountID, "INBOX", "2013-10-14", f.v, f.uid)
		if err := s.writeEML(filepath.Join(dataDir, rel), []byte(sampleEML)); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}
	if err := s.Backfill(ctx); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	for _, k := range []struct{ v, uid int64 }{{0, 900}, {5, 300}, {7, 2}} {
		if !exists(t, s.store, accountID, "INBOX", k.v, k.uid) {
			t.Errorf("message (uidvalidity %d, uid %d) not indexed", k.v, k.uid)
		}
	}
	if last, v := folderState(t, s, accountID, "INBOX"); last != 2 || v != 7 {
		t.Fatalf("watermark = (%d, %d), want (2, 7) from the newest generation", last, v)
	}
}

// TestMigrationKeepsMessages: the messages table rebuild in the UIDVALIDITY
// migration preserves existing rows and their ids.
func TestMigrationKeepsMessages(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "m.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(db, ".", 4); err != nil {
		t.Fatalf("migrate to 4: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO accounts (id, name, host, username, encrypted_password) VALUES (1, 'a', 'h', 'u', 'p')`,
		`INSERT INTO blobs (id, account_id, sha256, path, size) VALUES (1, 1, 's', 'p', 1)`,
		`INSERT INTO messages (id, account_id, folder, uid, blob_id, subject) VALUES (42, 1, 'INBOX', 7, 1, 'kept')`,
		`INSERT INTO folder_sync_state (account_id, folder, last_uid) VALUES (1, 'INBOX', 7)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	if err := goose.Up(db, "."); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	var subject string
	var uidValidity, uid int64
	if err := db.QueryRow(`SELECT subject, uidvalidity, uid FROM messages WHERE id = 42`).Scan(&subject, &uidValidity, &uid); err != nil {
		t.Fatalf("row lost in migration: %v", err)
	}
	if subject != "kept" || uidValidity != 0 || uid != 7 {
		t.Errorf("row = (%q, %d, %d), want (kept, 0, 7)", subject, uidValidity, uid)
	}
	var last, v int64
	if err := db.QueryRow(`SELECT last_uid, uidvalidity FROM folder_sync_state`).Scan(&last, &v); err != nil || last != 7 || v != 0 {
		t.Errorf("folder state = (%d, %d, %v), want (7, 0)", last, v, err)
	}
}
