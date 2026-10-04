package imap

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"katchup/internal/account"
	"katchup/internal/database"
	"katchup/internal/search"
)

type Store struct {
	db        *sql.DB
	accountSt *account.Store
	queries   *database.Queries
	// indexer receives a header-only search doc after each message is stored. It
	// defaults to search.NoopIndexer (search disabled) and is replaced via
	// SetIndexer when Meilisearch is configured. Never nil.
	indexer search.Indexer
}

func NewStore(db *sql.DB, accountSt *account.Store) (*Store, error) {
	return &Store{
		db:        db,
		accountSt: accountSt,
		queries:   database.New(db),
		indexer:   search.NoopIndexer{},
	}, nil
}

// SetIndexer swaps the header-only search indexer (called at startup when
// Meilisearch is configured). Passing nil restores the no-op indexer.
func (s *Store) SetIndexer(idx search.Indexer) {
	if idx == nil {
		idx = search.NoopIndexer{}
	}
	s.indexer = idx
}

// Blob is a domain view of one deduplicated encrypted file on disk.
type Blob struct {
	ID        int64
	AccountID int64
	Sha256    string
	Path      string
	Size      int64
	Refcount  int64
	CreatedAt string
}

// Message is a domain view of a messages row joined with its blob.
type Message struct {
	ID           int64
	AccountID    int64
	Folder       string
	UID          int64
	BlobID       int64
	MessageIDHdr string
	FuzzyFP      string
	FromAddr     string
	ToAddr       string
	Subject      string
	InternalDate string
	Size         int64
	CreatedAt    string
	BlobPath     string
	BlobSha256   string
}

// InsertMessageParams carries the fields for one messages row.
type InsertMessageParams struct {
	AccountID int64
	Folder    string
	// UIDValidity is the folder's UIDVALIDITY when the message was fetched (0 =
	// unknown, e.g. backfilled from a legacy file name).
	UIDValidity  int64
	UID          int64
	BlobID       int64
	MessageIDHdr string
	FuzzyFP      string
	FromAddr     string
	ToAddr       string
	Subject      string
	InternalDate string
	Size         int64
}

// GetBlobBySha returns the blob for (account, sha256) if one exists. The bool
// reports whether a blob was found; a missing blob is not an error.
func (s *Store) GetBlobBySha(ctx context.Context, accountID int64, sha string) (Blob, bool, error) {
	b, err := s.queries.GetBlobBySha(ctx, database.GetBlobByShaParams{
		AccountID: accountID,
		Sha256:    sha,
	})
	if err == sql.ErrNoRows {
		return Blob{}, false, nil
	}
	if err != nil {
		return Blob{}, false, fmt.Errorf("get blob by sha: %w", err)
	}
	return Blob{
		ID:        b.ID,
		AccountID: b.AccountID,
		Sha256:    b.Sha256,
		Path:      b.Path,
		Size:      b.Size,
		Refcount:  b.Refcount,
		CreatedAt: b.CreatedAt,
	}, true, nil
}

// UpsertBlob inserts a blob for (account, sha256) or bumps the refcount of the
// existing one, returning the blob id in both cases.
func (s *Store) UpsertBlob(ctx context.Context, accountID int64, sha, path string, size int64) (int64, error) {
	id, err := s.queries.UpsertBlob(ctx, database.UpsertBlobParams{
		AccountID: accountID,
		Sha256:    sha,
		Path:      path,
		Size:      size,
	})
	if err != nil {
		return 0, fmt.Errorf("upsert blob: %w", err)
	}
	return id, nil
}

// MessageExists reports whether a messages row already exists for
// (account, folder, uidvalidity, uid). Used to make the sync path idempotent.
func (s *Store) MessageExists(ctx context.Context, accountID int64, folder string, uidValidity, uid int64) (bool, error) {
	n, err := s.queries.ExistsMessage(ctx, database.ExistsMessageParams{
		AccountID:   accountID,
		Folder:      folder,
		Uidvalidity: uidValidity,
		Uid:         uid,
	})
	if err != nil {
		return false, fmt.Errorf("exists message: %w", err)
	}
	return n > 0, nil
}

// InsertMessage inserts a messages row (no-op on (account, folder, uidvalidity,
// uid) conflict).
func (s *Store) InsertMessage(ctx context.Context, p InsertMessageParams) error {
	if err := s.queries.InsertMessage(ctx, database.InsertMessageParams{
		AccountID:    p.AccountID,
		Folder:       p.Folder,
		Uidvalidity:  p.UIDValidity,
		Uid:          p.UID,
		BlobID:       p.BlobID,
		MessageIDHdr: nullStr(p.MessageIDHdr),
		FuzzyFp:      nullStr(p.FuzzyFP),
		FromAddr:     nullStr(p.FromAddr),
		ToAddr:       nullStr(p.ToAddr),
		Subject:      nullStr(p.Subject),
		InternalDate: nullStr(p.InternalDate),
		Size:         p.Size,
	}); err != nil {
		return fmt.Errorf("insert message: %w", err)
	}
	return nil
}

// InsertAndIndexMessage inserts a messages row (idempotent per account/folder/
// uidvalidity/uid)
// and then pushes a HEADER-ONLY doc to the search backend. Indexing is best-effort:
// a search-backend failure is logged and swallowed so it never fails the sync (the
// message is already durably stored). Only header fields are indexed — never the
// body — because search.Doc has no body field.
func (s *Store) InsertAndIndexMessage(ctx context.Context, p InsertMessageParams) error {
	if err := s.InsertMessage(ctx, p); err != nil {
		return err
	}
	if _, ok := s.indexer.(search.NoopIndexer); ok {
		return nil // search disabled — skip the id lookup entirely
	}
	id, err := s.queries.GetMessageIDByUID(ctx, database.GetMessageIDByUIDParams{
		AccountID:   p.AccountID,
		Folder:      p.Folder,
		Uidvalidity: p.UIDValidity,
		Uid:         p.UID,
	})
	if err != nil {
		slog.Warn("search index skipped: could not resolve message id", "account_id", p.AccountID, "folder", p.Folder, "uid", p.UID, "error", err)
		return nil
	}
	if err := s.indexer.Index(ctx, search.Doc{
		ID:        id,
		AccountID: p.AccountID,
		Folder:    p.Folder,
		MessageID: p.MessageIDHdr,
		From:      p.FromAddr,
		To:        p.ToAddr,
		Subject:   p.Subject,
		Date:      p.InternalDate,
	}); err != nil {
		slog.Warn("search index failed", "message_id", id, "error", err)
	}
	return nil
}

// SearchMessagesLike is the SQLite LIKE fallback for header-only search: it matches
// q.Text against subject/from only (never the encrypted body), within q's account
// and date bounds. Returns results newest-first, one page of q.Limit at q.Offset.
func (s *Store) SearchMessagesLike(ctx context.Context, q search.Query) ([]search.Result, error) {
	rows, err := s.queries.SearchMessagesLike(ctx, database.SearchMessagesLikeParams{
		AccountID: q.AccountID,
		Q:         nullStr("%" + q.Text + "%"),
		Since:     search.SQLTime(q.Since),
		Before:    search.SQLTime(q.Before),
		RowLimit:  int64(q.Limit),
		RowOffset: int64(q.Offset),
	})
	if err != nil {
		return nil, fmt.Errorf("search messages: %w", err)
	}
	out := make([]search.Result, len(rows))
	for i, r := range rows {
		out[i] = search.Result{
			ID:        r.ID,
			AccountID: r.AccountID,
			Folder:    r.Folder,
			MessageID: r.MessageIDHdr.String,
			From:      r.FromAddr.String,
			Subject:   r.Subject.String,
			Date:      r.InternalDate.String,
		}
	}
	return out, nil
}

// MessageFilter selects messages for ListMessages/CountMessages. Zero-valued
// fields don't filter.
type MessageFilter struct {
	AccountID int64
	// Date is one UTC day, "YYYY-MM-DD".
	Date string
	// Since (inclusive) and Before (exclusive) bound the internal date.
	Since, Before time.Time
}

// CountMessages returns the number of messages matching f.
func (s *Store) CountMessages(ctx context.Context, f MessageFilter) (int64, error) {
	return s.queries.CountMessages(ctx, database.CountMessagesParams{
		AccountID: f.AccountID,
		Date:      f.Date,
		Since:     search.SQLTime(f.Since),
		Before:    search.SQLTime(f.Before),
	})
}

// CountMessagesByAccount returns the total message count for one account.
func (s *Store) CountMessagesByAccount(ctx context.Context, accountID int64) (int64, error) {
	return s.queries.CountMessagesByAccount(ctx, accountID)
}

// ListMessages returns a page of messages (joined with their blob) matching f,
// newest first.
func (s *Store) ListMessages(ctx context.Context, f MessageFilter, limit, offset int64) ([]Message, error) {
	rows, err := s.queries.ListMessages(ctx, database.ListMessagesParams{
		AccountID: f.AccountID,
		Date:      f.Date,
		Since:     search.SQLTime(f.Since),
		Before:    search.SQLTime(f.Before),
		RowLimit:  limit,
		RowOffset: offset,
	})
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	out := make([]Message, len(rows))
	for i, r := range rows {
		out[i] = Message{
			ID:           r.ID,
			AccountID:    r.AccountID,
			Folder:       r.Folder,
			UID:          r.Uid,
			BlobID:       r.BlobID,
			MessageIDHdr: r.MessageIDHdr.String,
			FuzzyFP:      r.FuzzyFp.String,
			FromAddr:     r.FromAddr.String,
			ToAddr:       r.ToAddr.String,
			Subject:      r.Subject.String,
			InternalDate: r.InternalDate.String,
			Size:         r.Size,
			CreatedAt:    r.CreatedAt,
			BlobPath:     r.BlobPath,
			BlobSha256:   r.BlobSha256,
		}
	}
	return out, nil
}

// GetMessageWithBlob returns a single message joined with its blob by message id.
func (s *Store) GetMessageWithBlob(ctx context.Context, id int64) (Message, error) {
	r, err := s.queries.GetMessageWithBlob(ctx, id)
	if err != nil {
		return Message{}, fmt.Errorf("get message with blob: %w", err)
	}
	return Message{
		ID:           r.ID,
		AccountID:    r.AccountID,
		Folder:       r.Folder,
		UID:          r.Uid,
		BlobID:       r.BlobID,
		MessageIDHdr: r.MessageIDHdr.String,
		FuzzyFP:      r.FuzzyFp.String,
		FromAddr:     r.FromAddr.String,
		ToAddr:       r.ToAddr.String,
		Subject:      r.Subject.String,
		InternalDate: r.InternalDate.String,
		Size:         r.Size,
		CreatedAt:    r.CreatedAt,
		BlobPath:     r.BlobPath,
		BlobSha256:   r.BlobSha256,
	}, nil
}

// Archived is the archived-lookup result for one message (API read side).
type Archived struct {
	ID         int64  // katchup messages row id
	ArchivedAt string // when katchup stored it (messages.created_at)
	Sha256     string // content hash of the deduplicated blob
}

// LookupArchivedByMessageID resolves an archived message by its RFC5322
// Message-ID header. The bool reports whether a match was found; a miss is not
// an error.
func (s *Store) LookupArchivedByMessageID(ctx context.Context, messageID string) (Archived, bool, error) {
	r, err := s.queries.GetArchivedByMessageID(ctx, nullStr(messageID))
	if err == sql.ErrNoRows {
		return Archived{}, false, nil
	}
	if err != nil {
		return Archived{}, false, fmt.Errorf("lookup archived by message-id: %w", err)
	}
	return Archived{ID: r.ID, ArchivedAt: r.CreatedAt, Sha256: r.BlobSha256}, true, nil
}

// LookupArchivedByFuzzyFp resolves an archived message by its fuzzy fingerprint.
// The bool reports whether a match was found; a miss is not an error.
func (s *Store) LookupArchivedByFuzzyFp(ctx context.Context, fp string) (Archived, bool, error) {
	r, err := s.queries.GetArchivedByFuzzyFp(ctx, nullStr(fp))
	if err == sql.ErrNoRows {
		return Archived{}, false, nil
	}
	if err != nil {
		return Archived{}, false, fmt.Errorf("lookup archived by fuzzy_fp: %w", err)
	}
	return Archived{ID: r.ID, ArchivedAt: r.CreatedAt, Sha256: r.BlobSha256}, true, nil
}

// LookupArchived resolves a message by its Message-ID header first; if that
// misses and fp is non-empty, it falls back to the fuzzy fingerprint. The bool
// reports whether either matched; a miss is not an error.
//
// The Message-ID matches with or without angle brackets: IMAP sync stores the
// envelope value as-is ("<x@y>") while backfill stores it bare ("x@y"), and
// callers may pass either form.
func (s *Store) LookupArchived(ctx context.Context, messageID, fp string) (Archived, bool, error) {
	if bare := strings.TrimSpace(strings.Trim(strings.TrimSpace(messageID), "<>")); bare != "" {
		for _, id := range []string{"<" + bare + ">", bare} {
			a, found, err := s.LookupArchivedByMessageID(ctx, id)
			if err != nil || found {
				return a, found, err
			}
		}
	}
	if fp != "" {
		return s.LookupArchivedByFuzzyFp(ctx, fp)
	}
	return Archived{}, false, nil
}

// nullStr wraps a string as a valid sql.NullString (empty strings are stored as
// empty, not NULL, so computed values like fuzzy_fp are always retrievable).
func nullStr(s string) sql.NullString {
	return sql.NullString{String: s, Valid: true}
}

// GetLastSyncState returns the last synced UID and timestamp for an account
// across all folders. Used for backward compatibility.
func (s *Store) GetLastSyncState(ctx context.Context, accountID int64) (lastUID int64, lastSyncAt string, err error) {
	runs, err := s.accountSt.ListRecentRuns(ctx, accountID)
	if err != nil {
		return 0, "", fmt.Errorf("list recent runs: %w", err)
	}

	for _, run := range runs {
		if run.Status == "completed" && run.LastUid != nil {
			if run.FinishedAt != nil {
				lastSyncAt = *run.FinishedAt
			}
			return *run.LastUid, lastSyncAt, nil
		}
	}

	return 0, "", nil
}

// GetLastSyncStateForFolder returns the last synced UID for a specific folder
// from the folder_sync_state table. Returns 0 (full sync) when the folder has
// no record — deliberately no fallback to the account-level UID, because UIDs
// are per-mailbox and another folder's watermark would skip this folder's
// history. Re-fetches are deduplicated by the on-disk .eml files.
func (s *Store) GetLastSyncStateForFolder(ctx context.Context, accountID int64, folder string) (int64, error) {
	return s.accountSt.GetFolderLastUID(ctx, accountID, folder)
}

// AdoptUIDValidity stamps a folder's messages recorded before UIDVALIDITY was
// tracked (uidvalidity = 0) with the server's current value.
func (s *Store) AdoptUIDValidity(ctx context.Context, accountID int64, folder string, uidValidity int64) error {
	if err := s.queries.AdoptUIDValidity(ctx, database.AdoptUIDValidityParams{
		Uidvalidity: uidValidity,
		AccountID:   accountID,
		Folder:      folder,
	}); err != nil {
		return fmt.Errorf("adopt uidvalidity: %w", err)
	}
	return nil
}

func (s *Store) GetCurrentSyncRun(ctx context.Context, accountID int64) (*account.SyncRun, error) {
	runs, err := s.accountSt.ListRecentRuns(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("list recent runs: %w", err)
	}

	for _, run := range runs {
		if run.Status == "running" {
			return &run, nil
		}
	}

	return nil, nil
}
