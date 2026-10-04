package imap

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"katchup/internal/account"
)

func TestSweepTempFiles(t *testing.T) {
	s, _, dataDir := newTestSyncer(t)
	dir := filepath.Join(dataDir, "1", "INBOX")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".eml.tmp.123", ".eml.enc.tmp.456", "2026-01-01_1.eml.enc", "katchup.db"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}

	n, err := s.SweepTempFiles()
	if err != nil || n != 2 {
		t.Fatalf("SweepTempFiles = %d, %v; want 2", n, err)
	}
	left, _ := os.ReadDir(dir)
	if len(left) != 2 {
		t.Errorf("expected only the two real files left, got %v", left)
	}
}

// TestStopWaitsForTriggeredSync: Stop cancels a TriggerSync run and returns
// once it has finished, so shutdown doesn't close the DB under it.
func TestStopWaitsForTriggeredSync(t *testing.T) {
	store, accountID := newTestStore(t)
	s := NewSyncer(store, t.TempDir(), nil, store.accountSt)
	finished := make(chan struct{})
	s.runBody = func(ctx context.Context, _ account.Account, _ account.SyncRun) error {
		<-ctx.Done()
		time.Sleep(10 * time.Millisecond) // record progress
		close(finished)
		return ctx.Err()
	}

	if _, started, err := s.TriggerSync(context.Background(), accountID, 0); err != nil || !started {
		t.Fatalf("trigger: started=%v err=%v", started, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	select {
	case <-finished:
	default:
		t.Fatal("Stop returned before the sync finished")
	}
}
