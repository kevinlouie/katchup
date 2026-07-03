package imap

import (
	"context"
	"database/sql"
	"fmt"

	"katchup/internal/account"
)

type Store struct {
	db        *sql.DB
	accountSt *account.Store
}

func NewStore(db *sql.DB, accountSt *account.Store) (*Store, error) {
	return &Store{
		db:        db,
		accountSt: accountSt,
	}, nil
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
