package imap

import (
	"context"
	"testing"

	"katchup/internal/search"
)

type captureIndexer struct{ docs []search.Doc }

func (c *captureIndexer) Index(_ context.Context, d search.Doc) error {
	c.docs = append(c.docs, d)
	return nil
}

func TestReindexAll(t *testing.T) {
	store, accountID := newTestStore(t)
	ctx := context.Background()

	// Two stored messages sharing one blob.
	blobID, err := store.UpsertBlob(ctx, accountID, "sha-reindex", "1/INBOX/2013-10-14_1.eml", 100)
	if err != nil {
		t.Fatalf("upsert blob: %v", err)
	}
	for _, m := range []InsertMessageParams{
		{AccountID: accountID, Folder: "INBOX", UID: 1, BlobID: blobID, Subject: "First", FromAddr: "a@x.com", InternalDate: "2013-10-14T00:00:00Z"},
		{AccountID: accountID, Folder: "INBOX", UID: 2, BlobID: blobID, Subject: "Second", FromAddr: "b@x.com", InternalDate: "2013-10-15T00:00:00Z"},
	} {
		if err := store.InsertMessage(ctx, m); err != nil {
			t.Fatalf("insert message: %v", err)
		}
	}

	// Default indexer is Noop → reindex is a no-op.
	if n, err := store.ReindexAll(ctx); err != nil || n != 0 {
		t.Fatalf("noop reindex: got %d err=%v, want 0", n, err)
	}

	// With a real indexer, every stored message is pushed exactly once.
	cap := &captureIndexer{}
	store.SetIndexer(cap)
	n, err := store.ReindexAll(ctx)
	if err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if n != 2 || len(cap.docs) != 2 {
		t.Fatalf("expected 2 indexed docs, got n=%d docs=%d", n, len(cap.docs))
	}

	// Headers carried through; no body field exists on Doc (compile-time guarantee).
	subjects := map[string]bool{}
	for _, d := range cap.docs {
		subjects[d.Subject] = true
	}
	if !subjects["First"] || !subjects["Second"] {
		t.Errorf("missing subjects in indexed docs: %+v", cap.docs)
	}
}
