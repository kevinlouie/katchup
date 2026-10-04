package imap

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestPlanBatches(t *testing.T) {
	uids := []uint32{1, 2, 3, 4, 5, 6}
	tests := []struct {
		name     string
		sizes    map[uint32]int64
		maxCount int
		maxBytes int64
		want     [][]uint32
	}{
		{"count cap, sizes unknown", nil, 4, 100, [][]uint32{{1, 2, 3, 4}, {5, 6}}},
		{"byte cap", map[uint32]int64{1: 40, 2: 40, 3: 40, 4: 40, 5: 40, 6: 40}, 10, 100, [][]uint32{{1, 2}, {3, 4}, {5, 6}}},
		{"oversized message gets its own batch", map[uint32]int64{1: 10, 2: 500, 3: 10}, 10, 100, [][]uint32{{1}, {2}, {3, 4, 5, 6}}},
		{"exact fit stays together", map[uint32]int64{1: 50, 2: 50, 3: 1}, 10, 100, [][]uint32{{1, 2}, {3, 4, 5, 6}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := planBatches(uids, tt.sizes, tt.maxCount, tt.maxBytes); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("planBatches = %v, want %v", got, tt.want)
			}
		})
	}
	if got := planBatches(nil, nil, 10, 100); got != nil {
		t.Errorf("planBatches(nil) = %v, want nil", got)
	}
}

func TestIdleTimeout(t *testing.T) {
	if got := idleTimeout([]uint32{1}, nil); got != fetchTimeout {
		t.Errorf("unknown sizes: got %v, want %v", got, fetchTimeout)
	}
	sizes := map[uint32]int64{1: 1 << 10, 2: 100 * fetchMinRate}
	if got, want := idleTimeout([]uint32{1, 2}, sizes), fetchTimeout+100*time.Second; got != want {
		t.Errorf("large message: got %v, want %v", got, want)
	}
}

// TestSyncFolderEndToEnd runs the real sync path against an in-process IMAP
// server: every message is archived across several batches, and the folder's
// watermark and UIDVALIDITY are recorded.
func TestSyncFolderEndToEnd(t *testing.T) {
	s, accountID, _ := newTestSyncer(t)
	f := newFakeIMAP(t)
	useFakeIMAP(s, f)
	f.setUIDValidity(1700)
	ctx := context.Background()

	var uids []uint32
	for uid := uint32(1); uid <= fetchBatchSize+25; uid++ {
		uids = append(uids, uid*2) // gaps, like a real mailbox
	}
	f.add(uids...)

	if err := s.Run(ctx, accountID); err != nil {
		t.Fatalf("run: %v", err)
	}
	if n, _ := s.store.CountMessagesByAccount(ctx, accountID); n != int64(len(uids)) {
		t.Fatalf("archived %d messages, want %d", n, len(uids))
	}
	last := int64(uids[len(uids)-1])
	if l, v := folderState(t, s, accountID, "INBOX"); l != last || v != 1700 {
		t.Fatalf("folder state = (%d, %d), want (%d, 1700)", l, v, last)
	}

	// Incremental: only the new message is fetched next time.
	f.add(uint32(last + 1))
	if err := s.Run(ctx, accountID); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := f.fetches(uids[0]); got != 1 {
		t.Errorf("UID %d body fetched %d times, want 1", uids[0], got)
	}
	if !exists(t, s.store, accountID, "INBOX", 1700, last+1) {
		t.Error("new message not archived by the incremental run")
	}

	// The server renumbers the folder: the same UIDs now name new messages,
	// which must be archived rather than skipped.
	f.setUIDValidity(1800)
	if err := s.Run(ctx, accountID); err != nil {
		t.Fatalf("run after UIDVALIDITY change: %v", err)
	}
	if !exists(t, s.store, accountID, "INBOX", 1800, int64(uids[0])) {
		t.Error("message in the renumbered folder was not archived")
	}
	if l, v := folderState(t, s, accountID, "INBOX"); l != last+1 || v != 1800 {
		t.Fatalf("folder state after renumber = (%d, %d), want (%d, 1800)", l, v, last+1)
	}
}
