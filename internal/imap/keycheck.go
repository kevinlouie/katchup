package imap

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
)

// CheckMasterKey looks for data written under a different KATCHUP_MASTER_KEY
// setting than the current one — the key added, removed, or changed after
// mail was archived — and describes each problem found. None of these fix
// themselves: stored IMAP passwords and encrypted blobs are only readable with
// the key they were written with.
func (s *Syncer) CheckMasterKey(ctx context.Context) ([]string, error) {
	var problems []string

	accts, err := s.store.accountSt.ListAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	for _, a := range accts {
		if s.store.accountSt.MasterKeySet() {
			if _, err := s.store.accountSt.DecryptPassword(a.EncryptedPassword); err != nil {
				problems = append(problems, fmt.Sprintf(
					"account %q: stored IMAP password doesn't decrypt with this key (it was saved under a different key, or while the key was unset) — sync will fail until you re-enter the password",
					a.Name))
			}
		} else if _, err := s.store.accountSt.GetEncryption(ctx, a.ID); err == nil {
			problems = append(problems, fmt.Sprintf(
				"account %q was set up with a master key, so its stored IMAP password is ciphertext — sync will fail until the key is set again",
				a.Name))
		}
	}

	counts, err := s.store.queries.CountBlobsByFormat(ctx)
	if err != nil {
		return nil, fmt.Errorf("count blobs: %w", err)
	}
	switch {
	case s.keyWrapper == nil && counts.Encrypted > 0:
		problems = append(problems, fmt.Sprintf(
			"%d archived messages are encrypted but no master key is set — they can't be read, and new mail is stored in plaintext",
			counts.Encrypted))
	case s.keyWrapper != nil && counts.Encrypted > 0:
		path, err := s.store.queries.LatestEncryptedBlobPath(ctx)
		if err != nil && err != sql.ErrNoRows {
			return nil, fmt.Errorf("sample blob: %w", err)
		}
		if err == nil {
			// A missing file is a different problem; only judge a readable one.
			if _, err := ReadMessageBlob(s.dataDir, path, s.keyWrapper); err != nil && !errors.Is(err, os.ErrNotExist) {
				problems = append(problems, fmt.Sprintf(
					"archived messages don't decrypt with this key (%s: %v) — it differs from the key they were written with",
					path, err))
			}
		}
	}
	if s.keyWrapper != nil && counts.Plaintext > 0 {
		s.logger.Info("some mail was archived before encryption was enabled and stays plaintext on disk",
			"plaintext_messages", counts.Plaintext)
	}
	return problems, nil
}
