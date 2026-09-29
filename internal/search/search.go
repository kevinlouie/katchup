// Package search provides header-only full-text search over the message index.
//
// Only RFC5322 header metadata (from/to/subject/message-id/date/folder/account)
// is ever indexed — message bodies and attachment text stay encrypted at rest
// and are NEVER pushed to the search backend. When Meilisearch is unconfigured,
// the /search endpoint degrades to a SQLite LIKE over subject/from, so search is
// never a hard dependency.
package search

import (
	"context"
	"time"
)

// Doc is the header-only search document pushed to the backend on message store.
// It deliberately has NO body or attachment field — indexing headers only is the
// accepted plaintext scope (see v2-architecture.md "Search (S10) — headers only").
// ID is the katchup messages.id, which doubles as the Meili primary key and the
// link target (/browse/download/{id}).
type Doc struct {
	ID        int64  `json:"id"`
	AccountID int64  `json:"account_id"`
	Folder    string `json:"folder"`
	MessageID string `json:"message_id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Subject   string `json:"subject"`
	Date      string `json:"date"`
}

// Result is one search hit rendered on the /search page.
type Result struct {
	ID        int64
	AccountID int64
	Folder    string
	MessageID string
	From      string
	Subject   string
	Date      string
}

// Indexer pushes header-only docs to a search backend. The sync store calls it
// after persisting each message. Implementations must be safe for concurrent use.
type Indexer interface {
	Index(ctx context.Context, doc Doc) error
}

// Query is one header-only search request.
type Query struct {
	Text string
	// AccountID restricts to one account; 0 means all accounts.
	AccountID int64
	// Since (inclusive) and Before (exclusive) bound the message date; the zero
	// time leaves that side unbounded.
	Since, Before time.Time
	Limit, Offset int
}

// SQLTime renders t in the messages.internal_date format (RFC3339 UTC), so a
// string comparison orders correctly; the zero time renders as "".
func SQLTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// Searcher queries the backend.
type Searcher interface {
	Search(ctx context.Context, q Query) ([]Result, error)
}

// NoopIndexer is the indexer used when Meilisearch is unconfigured: indexing is a
// no-op and /search falls back to the SQLite LIKE query. It is the safe default
// so the sync path never fails on a missing search backend.
type NoopIndexer struct{}

func (NoopIndexer) Index(context.Context, Doc) error { return nil }
