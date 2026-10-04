package imap

import (
	"context"
	"os"
	"testing"
)

func failedAttempts(t *testing.T, s *Syncer, accountID int64, uidValidity int64, uid uint32) int64 {
	t.Helper()
	failed, err := s.store.FailedUIDs(context.Background(), accountID, "INBOX", uidValidity)
	if err != nil {
		t.Fatal(err)
	}
	return failed[uid]
}

// TestPoisonMessageIsEventuallySkipped: a message that always fails holds the
// watermark for maxUIDAttempts runs — without re-downloading the messages
// above it — and is then skipped so the watermark can move past it.
func TestPoisonMessageIsEventuallySkipped(t *testing.T) {
	s, accountID, _ := newTestSyncer(t)
	f := newFakeIMAP(t)
	useFakeIMAP(s, f)
	ctx := context.Background()

	f.add(1, 2, 3, 4, 5)
	f.setEmptyBody(3, true)

	for run := 1; run <= maxUIDAttempts; run++ {
		if err := s.Run(ctx, accountID); err == nil {
			t.Fatalf("run %d: expected an error for the poison message", run)
		}
		if got := failedAttempts(t, s, accountID, 1, 3); got != int64(run) {
			t.Fatalf("run %d: attempts = %d, want %d", run, got, run)
		}
		wantWM := int64(2)
		if run == maxUIDAttempts {
			wantWM = 5 // given up: no longer holds the watermark
		}
		if last, _ := folderState(t, s, accountID, "INBOX"); last != wantWM {
			t.Fatalf("run %d: watermark = %d, want %d", run, last, wantWM)
		}
	}
	// Messages above the poison one were downloaded exactly once.
	for _, uid := range []uint32{4, 5} {
		if got := f.fetches(uid); got != 1 {
			t.Errorf("UID %d body fetched %d times, want 1", uid, got)
		}
	}

	// Once given up on, it is not fetched again and the run is clean.
	if err := s.Run(ctx, accountID); err != nil {
		t.Fatalf("run after giving up: %v", err)
	}
	if got := f.fetches(3); got != maxUIDAttempts {
		t.Errorf("poison UID fetched %d times, want %d", got, maxUIDAttempts)
	}
	if n, _ := s.store.CountMessagesByAccount(ctx, accountID); n != 4 {
		t.Errorf("archived %d messages, want 4", n)
	}
}

// TestTransientMessageFailureRecovers: a message that fails once and then
// succeeds is archived and its failure record cleared.
func TestTransientMessageFailureRecovers(t *testing.T) {
	s, accountID, _ := newTestSyncer(t)
	f := newFakeIMAP(t)
	useFakeIMAP(s, f)
	ctx := context.Background()

	f.add(1, 2)
	f.setEmptyBody(1, true)
	if err := s.Run(ctx, accountID); err == nil {
		t.Fatal("expected an error on the first run")
	}
	if last, _ := folderState(t, s, accountID, "INBOX"); last != 0 {
		t.Fatalf("watermark = %d, want 0 while UID 1 is failing", last)
	}

	f.setEmptyBody(1, false)
	if err := s.Run(ctx, accountID); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if !exists(t, s.store, accountID, "INBOX", 1, 1) {
		t.Fatal("recovered message not archived")
	}
	if got := failedAttempts(t, s, accountID, 1, 1); got != 0 {
		t.Errorf("failure record not cleared, attempts = %d", got)
	}
	if last, _ := folderState(t, s, accountID, "INBOX"); last != 2 {
		t.Errorf("watermark = %d, want 2", last)
	}
}

// TestLocalWriteFailureIsNotCountedAgainstMessage: a failure on katchup's side
// (here an unwritable data dir) must never get a message skipped.
func TestLocalWriteFailureIsNotCountedAgainstMessage(t *testing.T) {
	s, accountID, dataDir := newTestSyncer(t)
	f := newFakeIMAP(t)
	useFakeIMAP(s, f)
	ctx := context.Background()
	f.add(1)

	if err := os.Chmod(dataDir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dataDir, 0700) })
	for run := 0; run < maxUIDAttempts+1; run++ {
		if err := s.Run(ctx, accountID); err == nil {
			t.Fatal("expected a write error")
		}
	}
	if got := failedAttempts(t, s, accountID, 1, 1); got != 0 {
		t.Fatalf("local failure counted against the message: attempts = %d", got)
	}

	os.Chmod(dataDir, 0700)
	if err := s.Run(ctx, accountID); err != nil {
		t.Fatalf("run after fixing the data dir: %v", err)
	}
	if !exists(t, s.store, accountID, "INBOX", 1, 1) {
		t.Fatal("message not archived once the data dir was writable")
	}
}
