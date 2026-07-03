package imap

import (
	"sync"
	"testing"
	"time"
)

func TestStaleRunAge(t *testing.T) {
	// Verify the stale run age is reasonable (30 minutes)
	if staleRunAge != 30*time.Minute {
		t.Fatalf("staleRunAge = %v, want 30m", staleRunAge)
	}
}

func TestWatermark(t *testing.T) {
	tests := []struct {
		name       string
		maxSuccess int64
		minFailed  int64
		want       int64
	}{
		{"no messages", 0, 0, 0},
		{"all succeeded", 42, 0, 42},
		{"all failed", 0, 7, 0},
		{"failure below max success caps watermark", 100, 50, 49},
		{"failure above max success keeps max success", 50, 100, 50},
		{"first message failed", 10, 1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := watermark(tt.maxSuccess, tt.minFailed); got != tt.want {
				t.Errorf("watermark(%d, %d) = %d, want %d", tt.maxSuccess, tt.minFailed, got, tt.want)
			}
		})
	}
}

func TestPerAccountLock(t *testing.T) {
	// Two accounts must lock independently; the same account must not
	// be lockable twice.
	s := &Syncer{}

	lockAny, _ := s.locks.LoadOrStore(int64(1), &sync.Mutex{})
	lock1 := lockAny.(*sync.Mutex)
	if !lock1.TryLock() {
		t.Fatal("first TryLock on account 1 should succeed")
	}
	defer lock1.Unlock()

	sameAny, _ := s.locks.LoadOrStore(int64(1), &sync.Mutex{})
	if sameAny.(*sync.Mutex).TryLock() {
		t.Fatal("second TryLock on account 1 should fail while held")
	}

	otherAny, _ := s.locks.LoadOrStore(int64(2), &sync.Mutex{})
	lock2 := otherAny.(*sync.Mutex)
	if !lock2.TryLock() {
		t.Fatal("TryLock on account 2 should succeed while account 1 is held")
	}
	lock2.Unlock()
}

// Compile-time checks for methods other packages depend on.
var _ = (*Store)(nil).GetLastSyncStateForFolder
var _ = (*Syncer)(nil).MarkAllStaleRuns
