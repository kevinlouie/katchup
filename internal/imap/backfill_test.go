package imap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"

	"katchup/internal/crypto"
)

const sampleEML = "From: Alice <alice@example.com>\r\n" +
	"To: bob@example.com\r\n" +
	"Subject: =?utf-8?q?Caf=C3=A9_meeting?=\r\n" +
	"Message-ID: <abc123@example.com>\r\n" +
	"Date: Mon, 14 Oct 2013 09:00:00 +0000\r\n" +
	"\r\n" +
	"Body text here.\r\n"

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// newTestSyncer builds a Syncer over the message_test.go store harness, with a
// real master-key wrapper so encrypted fixtures can be written and decrypted.
func newTestSyncer(t *testing.T) (*Syncer, int64, string) {
	t.Helper()
	store, accountID := newTestStore(t)
	kw, err := crypto.NewMasterKeyWrapper("test-master-key")
	if err != nil {
		t.Fatalf("key wrapper: %v", err)
	}
	dataDir := t.TempDir()
	s := NewSyncer(store, dataDir, kw, store.accountSt)
	return s, accountID, dataDir
}

func TestBackfillIndexesFromDisk(t *testing.T) {
	s, accountID, dataDir := newTestSyncer(t)
	ctx := context.Background()

	// Write two encrypted fixtures the way sync would: same account/folder, two UIDs.
	rel1 := s.relEmlPath(accountID, "INBOX", "2013-10-14", 6608)
	rel2 := s.relEmlPath(accountID, "INBOX", "2013-10-22", 6742)
	for _, rel := range []string{rel1, rel2} {
		if err := s.writeEML(filepath.Join(dataDir, rel), []byte(sampleEML)); err != nil {
			t.Fatalf("write fixture %s: %v", rel, err)
		}
	}

	if err := s.Backfill(ctx); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	// Two messages indexed (distinct UIDs), sharing ONE blob (identical content).
	if n, err := s.store.CountMessagesByAccount(ctx, accountID); err != nil || n != 2 {
		t.Fatalf("expected 2 messages, got %d (err=%v)", n, err)
	}
	blob, found, err := s.store.GetBlobBySha(ctx, accountID, sha256Hex([]byte(sampleEML)))
	if err != nil || !found {
		t.Fatalf("expected blob for content, found=%v err=%v", found, err)
	}
	if blob.Refcount != 2 {
		t.Errorf("expected refcount 2 for shared content, got %d", blob.Refcount)
	}

	// Header parsing: MIME subject decoded, Message-ID unwrapped, from = bare addr.
	msgs, err := s.store.ListMessages(ctx, accountID, "", 100, 0)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	var got *Message
	for i := range msgs {
		if msgs[i].UID == 6608 {
			got = &msgs[i]
			break
		}
	}
	if got == nil {
		t.Fatal("message UID 6608 not found")
	}
	if got.Subject != "Café meeting" {
		t.Errorf("subject not MIME-decoded: %q", got.Subject)
	}
	if got.MessageIDHdr != "abc123@example.com" {
		t.Errorf("message-id not unwrapped: %q", got.MessageIDHdr)
	}
	if got.FromAddr != "alice@example.com" {
		t.Errorf("from not normalized to bare addr: %q", got.FromAddr)
	}

	// Watermark advanced to the highest UID so the next IMAP sync is incremental.
	if uid, err := s.store.accountSt.GetFolderLastUID(ctx, accountID, "INBOX"); err != nil || uid != 6742 {
		t.Fatalf("expected watermark 6742, got %d (err=%v)", uid, err)
	}

	// Idempotent: a second run indexes nothing new.
	if err := s.Backfill(ctx); err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	if n, _ := s.store.CountMessagesByAccount(ctx, accountID); n != 2 {
		t.Errorf("second backfill changed message count: got %d, want 2", n)
	}
}
