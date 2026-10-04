package imap

import (
	"context"
	"sync"
	"testing"

	"katchup/internal/search"
)

// fakeIndexer records the docs it receives so tests can assert indexing happened
// on the store path without a live Meilisearch.
type fakeIndexer struct {
	mu   sync.Mutex
	docs []search.Doc
}

func (f *fakeIndexer) Index(_ context.Context, doc search.Doc) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.docs = append(f.docs, doc)
	return nil
}

// TestInsertAndIndexMessageInvokesIndexer verifies the S10 store path pushes a
// header-only doc (with the correct messages.id) to the injected indexer.
func TestInsertAndIndexMessageInvokesIndexer(t *testing.T) {
	store, accountID := newTestStore(t)
	ctx := context.Background()

	idx := &fakeIndexer{}
	store.SetIndexer(idx)

	blobID, err := store.UpsertBlob(ctx, accountID, "sha-idx", "1/INBOX/x.eml", 42)
	if err != nil {
		t.Fatalf("upsert blob: %v", err)
	}
	if err := store.InsertAndIndexMessage(ctx, InsertMessageParams{
		AccountID:    accountID,
		Folder:       "INBOX",
		UID:          99,
		BlobID:       blobID,
		MessageIDHdr: "<msg-99@example.com>",
		FromAddr:     "sender@example.com",
		ToAddr:       "rcpt@example.com",
		Subject:      "Indexed subject",
		InternalDate: "2026-07-01T12:00:00Z",
		Size:         42,
	}); err != nil {
		t.Fatalf("insert and index: %v", err)
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()
	if len(idx.docs) != 1 {
		t.Fatalf("expected 1 indexed doc, got %d", len(idx.docs))
	}
	doc := idx.docs[0]
	if doc.ID == 0 {
		t.Error("indexed doc id should be the messages row id, got 0")
	}
	if doc.Subject != "Indexed subject" {
		t.Errorf("expected subject to be indexed, got %q", doc.Subject)
	}
	if doc.MessageID != "<msg-99@example.com>" {
		t.Errorf("expected message id header indexed, got %q", doc.MessageID)
	}
	if doc.From != "sender@example.com" {
		t.Errorf("expected from indexed, got %q", doc.From)
	}
	if doc.AccountID != accountID || doc.Folder != "INBOX" {
		t.Errorf("expected account/folder indexed, got account=%d folder=%q", doc.AccountID, doc.Folder)
	}
}

// TestInsertAndIndexMessageNoopByDefault verifies the default (no Meili) path
// stores the message without indexing and never errors.
func TestInsertAndIndexMessageNoopByDefault(t *testing.T) {
	store, accountID := newTestStore(t)
	ctx := context.Background()

	blobID, err := store.UpsertBlob(ctx, accountID, "sha-noop", "1/INBOX/y.eml", 10)
	if err != nil {
		t.Fatalf("upsert blob: %v", err)
	}
	if err := store.InsertAndIndexMessage(ctx, InsertMessageParams{
		AccountID:    accountID,
		Folder:       "INBOX",
		UID:          100,
		BlobID:       blobID,
		Subject:      "hello",
		InternalDate: "2026-07-01T12:00:00Z",
		Size:         10,
	}); err != nil {
		t.Fatalf("insert and index (noop): %v", err)
	}

	exists, err := store.MessageExists(ctx, accountID, "INBOX", 0, 100)
	if err != nil || !exists {
		t.Fatalf("expected message stored despite no indexer, exists=%v err=%v", exists, err)
	}
}
