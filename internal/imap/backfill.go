package imap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"mime"
	"net/mail"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"katchup/internal/search"
)

// Backfill rebuilds the blobs + messages index (and the search index) from the
// encrypted .eml files already on disk, WITHOUT contacting any IMAP server. It
// exists to recover from a lost/reset database: the mail is durably stored as
// <dataDir>/<accountID>/<folder>/<date>_[<uidvalidity>_]<uid>.eml[.enc] files, so the index can
// be regenerated locally instead of re-downloading everything (which burns the
// provider's daily bandwidth cap — e.g. Gmail throttles at ~2.5GB/day).
//
// It is idempotent and resumable: a message already indexed for
// (account, folder, uidvalidity, uid) is skipped. After indexing a folder it
// sets the folder watermark to the highest UID of its newest UIDVALIDITY
// generation (UIDVALIDITY only ever increases), so the NEXT real IMAP sync is
// incremental (pulls only genuinely-new mail) rather than a full re-download —
// or, if the server has renumbered the folder since, a clean re-scan.
//
// Encrypted (.eml.enc) files need the key wrapper to be decrypted, to compute
// the content hash and read the header fields; without one they are skipped
// (and counted as failed). Plaintext (.eml) files are read as-is.
func (s *Syncer) Backfill(ctx context.Context) error {
	if s.keyWrapper == nil {
		s.logger.Warn("backfill: no master key — encrypted (.eml.enc) files will be skipped")
	}

	// Only backfill accounts that still have a row (FK on blobs/messages).
	// Deleted accounts keep theirs, so their archive is rebuilt too.
	ids, err := s.store.accountSt.ListAllAccountIDs(ctx)
	if err != nil {
		return fmt.Errorf("list accounts: %w", err)
	}
	known := make(map[int64]bool, len(ids))
	for _, id := range ids {
		known[id] = true
	}

	// Highest UID seen per (account, folder, uidvalidity), used to set the
	// resume watermark.
	type fkey struct {
		account     int64
		folder      string
		uidValidity int64
	}
	maxUID := make(map[fkey]int64)
	warnedMissing := make(map[int64]bool)

	var indexed, skipped, failed int64

	walkErr := filepath.WalkDir(s.dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".eml.enc") && !strings.HasSuffix(name, ".eml") {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}

		rel, rerr := filepath.Rel(s.dataDir, path)
		if rerr != nil {
			return nil
		}
		// <accountID>/<folder...>/<date>_[<uidvalidity>_]<uid>.eml[.enc]
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) < 3 {
			return nil // not an account/folder/file layout
		}
		accountID, perr := strconv.ParseInt(parts[0], 10, 64)
		if perr != nil {
			return nil
		}
		if !known[accountID] {
			if !warnedMissing[accountID] {
				s.logger.Warn("backfill: skipping files for unknown account (re-add it first)", "account_id", accountID)
				warnedMissing[accountID] = true
			}
			return nil
		}
		// Folder may itself contain separators (e.g. "[Gmail]/All Mail").
		folder := strings.Join(parts[1:len(parts)-1], "/")

		uidValidity, uid, ok := parseUIDsFromName(name)
		if !ok {
			s.logger.Warn("backfill: cannot parse UID from filename", "file", rel)
			return nil
		}
		key := fkey{accountID, folder, uidValidity}

		// Idempotent + resumable: already indexed → just track the watermark.
		if exists, eerr := s.store.MessageExists(ctx, accountID, folder, uidValidity, uid); eerr == nil && exists {
			skipped++
			if uid > maxUID[key] {
				maxUID[key] = uid
			}
			return nil
		}

		raw, derr := ReadMessageBlob(s.dataDir, rel, s.keyWrapper)
		if derr != nil {
			s.logger.Warn("backfill: decrypt failed, skipping", "file", rel, "error", derr)
			failed++
			return nil
		}
		if len(raw) == 0 {
			failed++
			return nil
		}

		sum := sha256.Sum256(raw)
		sha := hex.EncodeToString(sum[:])
		size := int64(len(raw))

		// Reuse an existing blob for identical content, else register this file.
		// The file already exists on disk, so we never write — only index.
		blobID, berr := s.store.UpsertBlob(ctx, accountID, sha, rel, size)
		if berr != nil {
			s.logger.Warn("backfill: upsert blob failed", "file", rel, "error", berr)
			failed++
			return nil
		}

		from, to, subject, internalDate := parseHeaders(raw, name)
		if ierr := s.store.InsertAndIndexMessage(ctx, InsertMessageParams{
			AccountID:    accountID,
			Folder:       folder,
			UIDValidity:  uidValidity,
			UID:          uid,
			BlobID:       blobID,
			MessageIDHdr: messageID(raw),
			FuzzyFP:      fuzzyFingerprint(from, internalDate, subject),
			FromAddr:     from,
			ToAddr:       to,
			Subject:      subject,
			InternalDate: internalDate,
			Size:         size,
		}); ierr != nil {
			s.logger.Warn("backfill: index message failed", "file", rel, "error", ierr)
			failed++
			return nil
		}

		if uid > maxUID[key] {
			maxUID[key] = uid
		}
		indexed++
		if indexed%1000 == 0 {
			s.logger.Info("backfill progress", "indexed", indexed, "skipped", skipped, "failed", failed)
		}
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("walk data dir: %w", walkErr)
	}

	// Advance each folder's watermark so the next IMAP sync is incremental. Only
	// the newest generation's UIDs mean anything to the server; if that is the
	// legacy 0, the next sync adopts the server's UIDVALIDITY.
	type folderKey struct {
		account int64
		folder  string
	}
	newest := make(map[folderKey]fkey)
	for k := range maxUID {
		fk := folderKey{k.account, k.folder}
		if cur, ok := newest[fk]; !ok || k.uidValidity > cur.uidValidity {
			newest[fk] = k
		}
	}
	for _, k := range newest {
		uid := maxUID[k]
		if err := s.store.accountSt.SetFolderSyncState(ctx, k.account, k.folder, k.uidValidity, uid); err != nil {
			s.logger.Warn("backfill: failed to set folder watermark", "account_id", k.account, "folder", k.folder, "error", err)
		} else {
			s.logger.Info("backfill: watermark set", "account_id", k.account, "folder", k.folder, "uidvalidity", k.uidValidity, "last_uid", uid)
		}
	}

	s.logger.Info("backfill done", "indexed", indexed, "skipped", skipped, "failed", failed)
	return nil
}

// ReindexAll re-pushes every stored message's header-only doc to the search
// backend. Use it to populate Meilisearch from an existing archive — e.g. after
// a `backfill` that ran without Meili reachable, or after enabling search on an
// already-populated database. It reads only the header fields already in the
// messages table (no decrypt, no IMAP) and is a no-op when search is disabled.
func (s *Store) ReindexAll(ctx context.Context) (int, error) {
	if _, ok := s.indexer.(search.NoopIndexer); ok {
		return 0, nil
	}
	// Deleted accounts included: their archived mail stays searchable.
	ids, err := s.accountSt.ListAllAccountIDs(ctx)
	if err != nil {
		return 0, fmt.Errorf("list accounts: %w", err)
	}
	const page = 500
	var indexed int
	for _, id := range ids {
		var offset int64
		for {
			if ctx.Err() != nil {
				return indexed, ctx.Err()
			}
			msgs, err := s.ListMessages(ctx, MessageFilter{AccountID: id}, page, offset)
			if err != nil {
				return indexed, fmt.Errorf("list messages: %w", err)
			}
			if len(msgs) == 0 {
				break
			}
			for i := range msgs {
				m := msgs[i]
				if err := s.indexer.Index(ctx, search.Doc{
					ID:        m.ID,
					AccountID: m.AccountID,
					Folder:    m.Folder,
					MessageID: m.MessageIDHdr,
					From:      m.FromAddr,
					To:        m.ToAddr,
					Subject:   m.Subject,
					Date:      m.InternalDate,
				}); err != nil {
					slog.Warn("reindex: index failed", "message_id", m.ID, "error", err)
					continue
				}
				indexed++
			}
			offset += int64(len(msgs))
			slog.Info("reindex progress", "indexed", indexed)
		}
	}
	return indexed, nil
}

// parseUIDsFromName extracts the UIDVALIDITY and UID from
// "<date>_<uidvalidity>_<uid>.eml[.enc]", or from the legacy "<date>_<uid>"
// form (UIDVALIDITY 0 = unknown). The date part contains '-' but no '_'.
func parseUIDsFromName(name string) (uidValidity, uid int64, ok bool) {
	base := strings.TrimSuffix(strings.TrimSuffix(name, ".enc"), ".eml")
	parts := strings.Split(base, "_")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, 0, false
	}
	uid, err := strconv.ParseInt(parts[len(parts)-1], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	if len(parts) == 3 {
		uidValidity, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return 0, 0, false
		}
	}
	return uidValidity, uid, true
}

// parseHeaders reads the indexed header fields from a raw RFC822 message. From/To
// are reduced to comma-separated email addresses (matching the IMAP sync path);
// the subject is MIME-word decoded for readable browse/search. internalDate falls
// back to the date encoded in the filename when the Date header is unparseable.
func parseHeaders(raw []byte, filename string) (from, to, subject, internalDate string) {
	internalDate = dateFromName(filename)
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return "", "", "", internalDate
	}
	from = joinMailAddrs(msg.Header.Get("From"))
	to = joinMailAddrs(msg.Header.Get("To"))
	subject = decodeMIME(msg.Header.Get("Subject"))
	if t, derr := msg.Header.Date(); derr == nil {
		internalDate = t.UTC().Format(time.RFC3339)
	}
	return from, to, subject, internalDate
}

func messageID(raw []byte) string {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	return strings.Trim(msg.Header.Get("Message-ID"), "<>")
}

// joinMailAddrs parses an address-list header and returns the bare email
// addresses, comma-separated. Falls back to the raw header if parsing fails.
func joinMailAddrs(header string) string {
	if header == "" {
		return ""
	}
	addrs, err := mail.ParseAddressList(header)
	if err != nil {
		return decodeMIME(header)
	}
	out := make([]string, len(addrs))
	for i, a := range addrs {
		out[i] = a.Address
	}
	return strings.Join(out, ", ")
}

var mimeDecoder = mime.WordDecoder{}

func decodeMIME(s string) string {
	if dec, err := mimeDecoder.DecodeHeader(s); err == nil {
		return dec
	}
	return s
}

// dateFromName pulls the YYYY-MM-DD prefix from "<date>_...eml[.enc]" and
// returns it as an RFC3339 timestamp at UTC midnight.
func dateFromName(name string) string {
	if i := strings.Index(name, "_"); i >= 10 {
		if d, err := time.Parse("2006-01-02", name[:i]); err == nil {
			return d.UTC().Format(time.RFC3339)
		}
	}
	return ""
}
