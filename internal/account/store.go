package account

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"

	"katchup/internal/crypto"
	"katchup/internal/database"
)

// Store handles all account and sync run database operations.
type Store struct {
	db        *sql.DB
	queries   *database.Queries
	mu        sync.RWMutex
	masterKey string
}

// Account represents a user's IMAP account with its configuration.
type Account struct {
	ID                int64
	Name              string
	Host              string
	Port              int64
	Username          string
	EncryptedPassword string
	UseSsl            bool
	Folders           []string
	CreatedAt         string
	UpdatedAt         string
}

// ListWithSync includes the is_syncing flag from the ListAccounts query.
type ListWithSync struct {
	Account
	IsSyncing bool
}

// New creates a new Store from an already-opened and migrated *sql.DB.
func New(db *sql.DB, masterKey string) (*Store, error) {
	store := &Store{
		db:        db,
		queries:   database.New(db),
		masterKey: masterKey,
	}

	slog.Info("account store initialized")
	return store, nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// CreateAccount inserts a new account and returns it with the generated ID.
func (s *Store) CreateAccount(ctx context.Context, name, host string, port int64, username, encryptedPassword string, useSsl bool, folders []string) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	foldersStr := ""
	if len(folders) > 0 {
		foldersStr = folders[0]
		for _, f := range folders[1:] {
			foldersStr += "," + f
		}
	} else {
		foldersStr = "INBOX"
	}

	useSslInt := int64(1)
	if !useSsl {
		useSslInt = int64(0)
	}

	acct, err := s.queries.CreateAccount(ctx, database.CreateAccountParams{
		Name:              name,
		Host:              host,
		Port:              port,
		Username:          username,
		EncryptedPassword: encryptedPassword,
		UseSsl:            useSslInt,
		Folders:           foldersStr,
	})
	if err != nil {
		return Account{}, fmt.Errorf("create account: %w", err)
	}

	return toAccount(acct), nil
}

// GetAccount returns a single account by ID.
func (s *Store) GetAccount(ctx context.Context, id int64) (Account, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	acct, err := s.queries.GetAccount(ctx, id)
	if err != nil {
		return Account{}, fmt.Errorf("get account: %w", err)
	}

	return toAccount(acct), nil
}

// ListAccounts returns all accounts sorted by name, with sync status.
func (s *Store) ListAccounts(ctx context.Context) ([]ListWithSync, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.queries.ListAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}

	result := make([]ListWithSync, len(rows))
	for i, row := range rows {
		result[i] = ListWithSync{
			Account:   toAccount(accountFromRow(row)),
			IsSyncing: row.IsSyncing > 0,
		}
	}

	return result, nil
}

// UpdateAccount updates an existing account and returns the updated account.
func (s *Store) UpdateAccount(ctx context.Context, id int64, name, host string, port int64, username, encryptedPassword string, useSsl bool, folders []string) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	foldersStr := ""
	if len(folders) > 0 {
		foldersStr = folders[0]
		for _, f := range folders[1:] {
			foldersStr += "," + f
		}
	} else {
		foldersStr = "INBOX"
	}

	useSslInt := int64(1)
	if !useSsl {
		useSslInt = int64(0)
	}

	acct, err := s.queries.UpdateAccount(ctx, database.UpdateAccountParams{
		Name:              name,
		Host:              host,
		Port:              port,
		Username:          username,
		EncryptedPassword: encryptedPassword,
		UseSsl:            useSslInt,
		Folders:           foldersStr,
		ID:                id,
	})
	if err != nil {
		return Account{}, fmt.Errorf("update account: %w", err)
	}

	return toAccount(acct), nil
}

// DeleteAccount removes an account by ID. Cascade deletes sync_runs.
func (s *Store) DeleteAccount(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.queries.DeleteAccount(ctx, id); err != nil {
		return fmt.Errorf("delete account: %w", err)
	}

	return nil
}

// CreateSyncRun creates a new sync run record for an account.
func (s *Store) CreateSyncRun(ctx context.Context, accountID int64) (SyncRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	run, err := s.queries.CreateSyncRun(ctx, accountID)
	if err != nil {
		return SyncRun{}, fmt.Errorf("create sync run: %w", err)
	}

	return toSyncRun(run), nil
}

// UpsertFolderSyncState sets the last synced UID for a specific (account, folder).
// Uses INSERT OR REPLACE so it's safe to call repeatedly.
func (s *Store) UpsertFolderSyncState(ctx context.Context, accountID int64, folder string, lastUID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO folder_sync_state (account_id, folder, last_uid, updated_at)
		 VALUES (?1, ?2, ?3, datetime('now'))
		 ON CONFLICT(account_id, folder) DO UPDATE SET
			 last_uid = excluded.last_uid,
			 updated_at = datetime('now')`,
		accountID, folder, lastUID)
	if err != nil {
		return fmt.Errorf("upsert folder sync state: %w", err)
	}
	return nil
}

// SetFolderSyncState records a folder's UIDVALIDITY together with its
// watermark. Used when the generation is first seen or changes, so the two
// are always updated as a pair.
func (s *Store) SetFolderSyncState(ctx context.Context, accountID int64, folder string, uidValidity, lastUID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO folder_sync_state (account_id, folder, uidvalidity, last_uid, updated_at)
		 VALUES (?1, ?2, ?3, ?4, datetime('now'))
		 ON CONFLICT(account_id, folder) DO UPDATE SET
			 uidvalidity = excluded.uidvalidity,
			 last_uid = excluded.last_uid,
			 updated_at = datetime('now')`,
		accountID, folder, uidValidity, lastUID)
	if err != nil {
		return fmt.Errorf("set folder sync state: %w", err)
	}
	return nil
}

// GetFolderSyncState returns the watermark and UIDVALIDITY recorded for a
// folder. Both are 0 if no record exists; a 0 UIDVALIDITY on an existing row
// means it predates UIDVALIDITY tracking.
func (s *Store) GetFolderSyncState(ctx context.Context, accountID int64, folder string) (lastUID, uidValidity int64, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	err = s.db.QueryRowContext(ctx,
		"SELECT last_uid, uidvalidity FROM folder_sync_state WHERE account_id = ?1 AND folder = ?2",
		accountID, folder).Scan(&lastUID, &uidValidity)
	if err == sql.ErrNoRows {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("get folder sync state: %w", err)
	}
	return lastUID, uidValidity, nil
}

// GetFolderLastUID returns the last synced UID for a specific (account, folder).
// Returns 0 if no record exists.
func (s *Store) GetFolderLastUID(ctx context.Context, accountID int64, folder string) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var lastUID int64
	err := s.db.QueryRowContext(ctx,
		"SELECT last_uid FROM folder_sync_state WHERE account_id = ?1 AND folder = ?2",
		accountID, folder).Scan(&lastUID)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, fmt.Errorf("get folder last uid: %w", err)
	}
	return lastUID, nil
}
func (s *Store) UpdateSyncRunStatus(ctx context.Context, runID int64, emailsBackedUp int64, errorsStr string, status string, lastUid int64) (SyncRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var errVal sql.NullString
	if errorsStr != "" {
		errVal = sql.NullString{String: errorsStr, Valid: true}
	}

	var uidVal sql.NullInt64
	if lastUid > 0 {
		uidVal = sql.NullInt64{Int64: lastUid, Valid: true}
	}

	run, err := s.queries.UpdateSyncRunStatus(ctx, database.UpdateSyncRunStatusParams{
		EmailsBackedUp: emailsBackedUp,
		Errors:         errVal,
		Status:         status,
		LastUid:        uidVal,
		ID:             runID,
	})
	if err != nil {
		return SyncRun{}, fmt.Errorf("update sync run status: %w", err)
	}

	return toSyncRun(run), nil
}

// MarkSyncRunProgress records incremental progress (emails written so far and
// the current watermark) on an in-flight run without ending it.
func (s *Store) MarkSyncRunProgress(ctx context.Context, runID, emailsBackedUp, lastUid int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var uidVal sql.NullInt64
	if lastUid > 0 {
		uidVal = sql.NullInt64{Int64: lastUid, Valid: true}
	}

	return s.queries.MarkSyncRunProgress(ctx, database.MarkSyncRunProgressParams{
		EmailsBackedUp: emailsBackedUp,
		LastUid:        uidVal,
		ID:             runID,
	})
}

// UpdateSyncRunFolderLastUID sets the per-folder last UID for a completed run.
func (s *Store) UpdateSyncRunFolderLastUID(ctx context.Context, runID int64, folder string, folderLastUID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx,
		"UPDATE sync_runs SET folder_last_uid = ?1 WHERE id = ?2 AND folder = ?3",
		folderLastUID, runID, folder)
	if err != nil {
		return fmt.Errorf("update folder last uid: %w", err)
	}
	return nil
}

// ListRecentRuns returns the 20 most recent sync runs for an account.
func (s *Store) ListRecentRuns(ctx context.Context, accountID int64) ([]SyncRun, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	runs, err := s.queries.ListRecentRuns(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("list recent runs: %w", err)
	}

	result := make([]SyncRun, len(runs))
	for i, run := range runs {
		result[i] = toSyncRun(run)
	}

	return result, nil
}

// toAccount converts a database.Account to our domain Account.
func toAccount(acct database.Account) Account {
	return Account{
		ID:                acct.ID,
		Name:              acct.Name,
		Host:              acct.Host,
		Port:              acct.Port,
		Username:          acct.Username,
		EncryptedPassword: acct.EncryptedPassword,
		UseSsl:            acct.UseSsl == 1,
		Folders:           splitFolders(acct.Folders),
		CreatedAt:         acct.CreatedAt,
		UpdatedAt:         acct.UpdatedAt,
	}
}

// toSyncRun converts a database.SyncRun to our domain SyncRun.
func toSyncRun(run database.SyncRun) SyncRun {
	sr := SyncRun{
		ID:             run.ID,
		AccountID:      run.AccountID,
		StartedAt:      run.StartedAt,
		EmailsBackedUp: run.EmailsBackedUp,
		Status:         run.Status,
		CreatedAt:      run.CreatedAt,
	}
	if run.FinishedAt.Valid {
		sr.FinishedAt = &run.FinishedAt.String
	}
	if run.Errors.Valid {
		sr.Errors = &run.Errors.String
	}
	if run.LastUid.Valid {
		sr.LastUid = &run.LastUid.Int64
	}
	return sr
}

// splitFolders splits a comma-separated folder string into a slice.
func splitFolders(s string) []string {
	if s == "" {
		return []string{"INBOX"}
	}
	result := []string{}
	for _, f := range split(s, ',') {
		f = trim(f)
		if f != "" {
			result = append(result, f)
		}
	}
	if len(result) == 0 {
		return []string{"INBOX"}
	}
	return result
}

// split is a simple string splitter (no strconv import needed).
func split(s string, sep rune) []string {
	var result []string
	var current []rune
	for _, r := range s {
		if r == sep {
			result = append(result, string(current))
			current = nil
		} else {
			current = append(current, r)
		}
	}
	if len(current) > 0 {
		result = append(result, string(current))
	}
	return result
}

// trim is a simple string trim (no strings import needed).
func trim(s string) string {
	start := 0
	end := len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}

// Encryption represents encryption metadata for an account.
type Encryption struct {
	ID                        int64
	AccountID                 int64
	YubikeySlotID             string
	SlotFingerprint           string
	EncryptedContentKeyPrefix string
	CreatedAt                 string
}

func toEncryption(enc database.AccountEncryption) Encryption {
	e := Encryption{
		ID:              enc.ID,
		AccountID:       enc.AccountID,
		YubikeySlotID:   enc.YubikeySlotID,
		SlotFingerprint: enc.SlotFingerprint,
		CreatedAt:       enc.CreatedAt,
	}
	if enc.EncryptedContentKeyPrefix.Valid {
		e.EncryptedContentKeyPrefix = enc.EncryptedContentKeyPrefix.String
	}
	return e
}

// UpsertEncryption stores or updates encryption metadata for an account.
func (s *Store) UpsertEncryption(ctx context.Context, accountID int64, slotID, fingerprint, encryptedKeyPrefix string) (Encryption, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var prefix sql.NullString
	if encryptedKeyPrefix != "" {
		prefix = sql.NullString{String: encryptedKeyPrefix, Valid: true}
	}

	enc, err := s.queries.UpsertAccountEncryption(ctx, database.UpsertAccountEncryptionParams{
		AccountID:                 accountID,
		YubikeySlotID:             slotID,
		SlotFingerprint:           fingerprint,
		EncryptedContentKeyPrefix: prefix,
	})
	if err != nil {
		return Encryption{}, fmt.Errorf("upsert encryption: %w", err)
	}

	return toEncryption(enc), nil
}

// GetEncryption returns encryption metadata for an account.
func (s *Store) GetEncryption(ctx context.Context, accountID int64) (Encryption, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	enc, err := s.queries.GetAccountEncryption(ctx, accountID)
	if err != nil {
		return Encryption{}, fmt.Errorf("get encryption: %w", err)
	}

	return toEncryption(enc), nil
}

// GetEncryptionByPrefix looks up encryption metadata by account and encrypted key prefix.
func (s *Store) GetEncryptionByPrefix(ctx context.Context, accountID int64, prefix string) (Encryption, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	enc, err := s.queries.GetAccountEncryptionByPrefix(ctx, database.GetAccountEncryptionByPrefixParams{
		AccountID:                 accountID,
		EncryptedContentKeyPrefix: sql.NullString{String: prefix, Valid: true},
	})
	if err != nil {
		return Encryption{}, fmt.Errorf("get encryption by prefix: %w", err)
	}

	return toEncryption(enc), nil
}

// ListEncryption returns all encryption metadata rows.
func (s *Store) ListEncryption(ctx context.Context) ([]Encryption, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	encs, err := s.queries.ListAccountEncryption(ctx)
	if err != nil {
		return nil, fmt.Errorf("list encryption: %w", err)
	}

	result := make([]Encryption, len(encs))
	for i, enc := range encs {
		result[i] = toEncryption(enc)
	}

	return result, nil
}

// DeleteEncryption removes encryption metadata for an account.
func (s *Store) DeleteEncryption(ctx context.Context, accountID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.queries.DeleteAccountEncryption(ctx, accountID); err != nil {
		return fmt.Errorf("delete encryption: %w", err)
	}

	return nil
}

// MasterKeySet returns true if a master key is configured.
func (s *Store) MasterKeySet() bool {
	return s.masterKey != ""
}

// EncryptPassword encrypts a plaintext password using the master key.
func (s *Store) EncryptPassword(plaintext string) (string, error) {
	cfg := s.masterKey
	if cfg == "" {
		return "", fmt.Errorf("account: no master key configured")
	}
	return crypto.EncryptPassword(cfg, plaintext)
}

// DecryptPassword decrypts a stored encrypted password using the master key.
func (s *Store) DecryptPassword(encrypted string) (string, error) {
	cfg := s.masterKey
	if cfg == "" {
		return "", fmt.Errorf("account: no master key configured")
	}
	return crypto.DecryptPassword(cfg, encrypted)
}

// accountFromRow converts a ListAccountsRow to a database.Account (without is_syncing).
func accountFromRow(row database.ListAccountsRow) database.Account {
	return database.Account{
		ID:                row.ID,
		Name:              row.Name,
		Host:              row.Host,
		Port:              row.Port,
		Username:          row.Username,
		EncryptedPassword: row.EncryptedPassword,
		UseSsl:            row.UseSsl,
		Folders:           row.Folders,
		CreatedAt:         row.CreatedAt,
		UpdatedAt:         row.UpdatedAt,
	}
}

// SyncRun represents a sync run record.
type SyncRun struct {
	ID             int64   `json:"id"`
	AccountID      int64   `json:"account_id"`
	StartedAt      string  `json:"started_at"`
	FinishedAt     *string `json:"finished_at,omitempty"`
	EmailsBackedUp int64   `json:"emails_backed_up"`
	Errors         *string `json:"errors,omitempty"`
	Status         string  `json:"status"`
	LastUid        *int64  `json:"last_uid,omitempty"`
	CreatedAt      string  `json:"created_at"`
}
