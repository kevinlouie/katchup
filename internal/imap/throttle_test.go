package imap

import (
	"context"
	"testing"
	"time"
)

func TestIsThrottleError(t *testing.T) {
	throttle := []string{
		"folder INBOX: search: [ALERT] Account exceeded the bandwidth limits",
		"login: Too many simultaneous connections",
		"[OVERQUOTA] quota exceeded",
		"Too many login attempts",
	}
	for _, m := range throttle {
		if !isThrottleError(m) {
			t.Errorf("expected throttle for %q", m)
		}
	}
	notThrottle := []string{
		"folder INBOX: UID 5: read body: unexpected EOF",
		"dial TLS: connection refused",
		"login: authentication failed",
		"",
	}
	for _, m := range notThrottle {
		if isThrottleError(m) {
			t.Errorf("did not expect throttle for %q", m)
		}
	}
}

// seedThrottledRun creates a completed run marked "throttled" that finished now,
// so the account should be in cooldown.
func seedThrottledRun(t *testing.T, s *Syncer, accountID int64) {
	t.Helper()
	ctx := context.Background()
	run, err := s.store.accountSt.CreateSyncRun(ctx, accountID)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := s.store.accountSt.UpdateSyncRunStatus(ctx, run.ID, 0, "bandwidth limit", "throttled", 0); err != nil {
		t.Fatalf("mark throttled: %v", err)
	}
}

func TestThrottledUntilAndSkip(t *testing.T) {
	s, accountID, bodyRuns, release := newTriggerSyncer(t)
	close(release) // runBody won't block if it ever runs
	ctx := context.Background()

	// No runs yet → not throttled.
	if _, yes := s.throttledUntil(ctx, accountID); yes {
		t.Fatal("unexpected throttle with no runs")
	}

	seedThrottledRun(t, s, accountID)

	until, yes := s.throttledUntil(ctx, accountID)
	if !yes {
		t.Fatal("expected account to be throttled")
	}
	if time.Until(until) <= 0 {
		t.Errorf("cooldown should be in the future, got %v", until)
	}

	// A trigger during cooldown must NOT start a sync.
	_, started, err := s.TriggerSync(ctx, accountID, 0)
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if started {
		t.Error("trigger started a sync while throttled")
	}
	if n := bodyRuns.Load(); n != 0 {
		t.Errorf("sync body ran %d times while throttled, want 0", n)
	}

	// Disabling the cooldown lifts the skip.
	s.ThrottleCooldown = 0
	if _, yes := s.throttledUntil(ctx, accountID); yes {
		t.Error("cooldown disabled but still reported throttled")
	}
}
