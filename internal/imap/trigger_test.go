package imap

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"katchup/internal/account"
)

// newTriggerSyncer builds a Syncer over a fresh DB+account with a stubbed
// runBody so the trigger/coalesce coordination can be exercised without a live
// IMAP server. The returned func lets a test block a running sync until it
// signals completion; bodyRuns counts how many times the sync body executed
// (i.e. how many runs actually started).
func newTriggerSyncer(t *testing.T) (*Syncer, int64, *atomic.Int64, chan struct{}) {
	t.Helper()

	store, accountID := newTestStore(t)
	s := NewSyncer(store, t.TempDir(), nil, store.accountSt)

	var bodyRuns atomic.Int64
	release := make(chan struct{})
	s.runBody = func(ctx context.Context, acct account.Account, syncRun account.SyncRun) error {
		bodyRuns.Add(1)
		<-release // hold the run open (and the per-account lock) until released
		_, err := store.accountSt.UpdateSyncRunStatus(ctx, syncRun.ID, 0, "", "completed", 0)
		return err
	}

	return s, accountID, &bodyRuns, release
}

func countRuns(t *testing.T, s *Syncer, accountID int64) int {
	t.Helper()
	runs, err := s.store.accountSt.ListRecentRuns(context.Background(), accountID)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	return len(runs)
}

// TestTriggerConcurrentSingleRun: many concurrent triggers must produce exactly
// one sync run.
func TestTriggerConcurrentSingleRun(t *testing.T) {
	s, accountID, bodyRuns, release := newTriggerSyncer(t)
	ctx := context.Background()

	const n = 8
	var wg sync.WaitGroup
	ids := make([]int64, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id, _, err := s.TriggerSync(ctx, accountID, 30*time.Second)
			if err != nil {
				t.Errorf("trigger %d: %v", i, err)
			}
			ids[i] = id
		}(i)
	}
	wg.Wait()

	if got := bodyRuns.Load(); got != 1 {
		t.Fatalf("expected exactly 1 sync body to run, got %d", got)
	}
	if n := countRuns(t, s, accountID); n != 1 {
		t.Fatalf("expected exactly 1 sync_run row, got %d", n)
	}
	// Every caller should have observed the same (only) run id.
	for i, id := range ids {
		if id != ids[0] {
			t.Fatalf("trigger %d returned run id %d, want %d", i, id, ids[0])
		}
	}

	close(release)
}

// TestTriggerJoinsRunning: a trigger issued while a sync is running must join the
// in-flight run (no new run, same id).
func TestTriggerJoinsRunning(t *testing.T) {
	s, accountID, bodyRuns, release := newTriggerSyncer(t)
	ctx := context.Background()

	firstID, started, err := s.TriggerSync(ctx, accountID, 30*time.Second)
	if err != nil || !started {
		t.Fatalf("first trigger: id=%d started=%v err=%v", firstID, started, err)
	}

	// Wait for the run body to actually be executing (lock held).
	waitFor(t, func() bool { return bodyRuns.Load() == 1 })

	secondID, started2, err := s.TriggerSync(ctx, accountID, 30*time.Second)
	if err != nil {
		t.Fatalf("second trigger: %v", err)
	}
	if started2 {
		t.Fatal("second trigger should have joined, not started a new run")
	}
	if secondID != firstID {
		t.Fatalf("second trigger returned id %d, want joined id %d", secondID, firstID)
	}
	if got := bodyRuns.Load(); got != 1 {
		t.Fatalf("expected 1 sync body running, got %d", got)
	}
	if n := countRuns(t, s, accountID); n != 1 {
		t.Fatalf("expected 1 sync_run row, got %d", n)
	}

	close(release)
}

// TestTriggerCoalesceWindow: a trigger within the coalesce window of a finished
// run must not start a new run; outside the window it must.
func TestTriggerCoalesceWindow(t *testing.T) {
	s, accountID, bodyRuns, release := newTriggerSyncer(t)
	ctx := context.Background()

	// First run: start it, let it finish.
	firstID, started, err := s.TriggerSync(ctx, accountID, time.Hour)
	if err != nil || !started {
		t.Fatalf("first trigger: id=%d started=%v err=%v", firstID, started, err)
	}
	waitFor(t, func() bool { return bodyRuns.Load() == 1 })
	close(release) // let the run finish
	waitForRunFinished(t, s, accountID, firstID)

	// Within the (very large) window: must coalesce onto the finished run.
	coalescedID, started2, err := s.TriggerSync(ctx, accountID, time.Hour)
	if err != nil {
		t.Fatalf("coalesced trigger: %v", err)
	}
	if started2 {
		t.Fatal("trigger within coalesce window should not start a new run")
	}
	if coalescedID != firstID {
		t.Fatalf("coalesced trigger returned id %d, want %d", coalescedID, firstID)
	}
	if got := bodyRuns.Load(); got != 1 {
		t.Fatalf("expected still 1 sync body run, got %d", got)
	}
	if n := countRuns(t, s, accountID); n != 1 {
		t.Fatalf("expected still 1 sync_run row, got %d", n)
	}

	// With a zero window, coalescing is disabled: a new run must start.
	release2 := make(chan struct{})
	s.runBody = func(ctx context.Context, acct account.Account, syncRun account.SyncRun) error {
		bodyRuns.Add(1)
		<-release2
		_, err := s.store.accountSt.UpdateSyncRunStatus(ctx, syncRun.ID, 0, "", "completed", 0)
		return err
	}
	newID, started3, err := s.TriggerSync(ctx, accountID, 0)
	if err != nil || !started3 {
		t.Fatalf("zero-window trigger: id=%d started=%v err=%v", newID, started3, err)
	}
	if newID == firstID {
		t.Fatalf("zero-window trigger should start a new run, got same id %d", newID)
	}
	waitFor(t, func() bool { return bodyRuns.Load() == 2 })
	if n := countRuns(t, s, accountID); n != 2 {
		t.Fatalf("expected 2 sync_run rows after new run, got %d", n)
	}
	close(release2)
}

// TestScheduledAndTriggerNoDoubleRun: a scheduled Run and an on-demand trigger
// racing must not both run the same account.
func TestScheduledAndTriggerNoDoubleRun(t *testing.T) {
	s, accountID, bodyRuns, release := newTriggerSyncer(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		// Run returns "already running" as an error when it loses the race; that
		// is expected and not a failure.
		_ = s.Run(ctx, accountID)
	}()
	go func() {
		defer wg.Done()
		if _, _, err := s.TriggerSync(ctx, accountID, 30*time.Second); err != nil {
			t.Errorf("trigger: %v", err)
		}
	}()

	// One of the two acquired the lock and is executing; the other bailed.
	waitFor(t, func() bool { return bodyRuns.Load() == 1 })
	// Give the loser a moment to also (incorrectly) start, if it were going to.
	time.Sleep(20 * time.Millisecond)
	if got := bodyRuns.Load(); got != 1 {
		t.Fatalf("scheduled + trigger double-ran: %d bodies", got)
	}
	if n := countRuns(t, s, accountID); n != 1 {
		t.Fatalf("expected 1 sync_run row, got %d", n)
	}

	close(release)
	wg.Wait()
}

// waitFor polls cond for up to ~2s.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// waitForRunFinished polls until the given run has a non-running status.
func waitForRunFinished(t *testing.T, s *Syncer, accountID, runID int64) {
	t.Helper()
	waitFor(t, func() bool {
		runs, err := s.store.accountSt.ListRecentRuns(context.Background(), accountID)
		if err != nil {
			return false
		}
		for _, r := range runs {
			if r.ID == runID {
				return r.Status != "running"
			}
		}
		return false
	})
}
