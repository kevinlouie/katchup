package imap

import (
	"context"
	"path/filepath"
	"testing"
)

// TestDeletedAccountKeepsArchive: deleting an account keeps its archived mail
// indexed and lets backfill rebuild it, but it is no longer synced.
func TestDeletedAccountKeepsArchive(t *testing.T) {
	s, accountID, dataDir := newTestSyncer(t)
	ctx := context.Background()

	rel := s.relEmlPath(accountID, "INBOX", "2013-10-14", 9, 1)
	if err := s.writeEML(filepath.Join(dataDir, rel), []byte(sampleEML)); err != nil {
		t.Fatal(err)
	}
	if err := s.Backfill(ctx); err != nil {
		t.Fatal(err)
	}

	if err := s.store.accountSt.DeleteAccount(ctx, accountID); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.store.CountMessagesByAccount(ctx, accountID); n != 1 {
		t.Fatalf("archive lost on delete: %d messages, want 1", n)
	}
	if err := s.Run(ctx, accountID); err == nil {
		t.Fatal("expected a deleted account not to sync")
	}

	// Simulate a lost index: backfill still recognises the deleted account.
	if _, err := s.store.db.Exec("DELETE FROM messages"); err != nil {
		t.Fatal(err)
	}
	if err := s.Backfill(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.store.CountMessagesByAccount(ctx, accountID); n != 1 {
		t.Fatalf("backfill skipped the deleted account: %d messages, want 1", n)
	}
}
