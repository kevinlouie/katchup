package imap

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"katchup/internal/account"
	"katchup/internal/crypto"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

const (
	connectionTimeout = 30 * time.Second
	fetchTimeout      = 120 * time.Second
	fetchBatchSize    = 200
	maxRetries        = 3
	retryBaseDelay    = 1 * time.Second
	maxErrors         = 5
	maxErrorsLen      = 1000
	staleRunAge       = 30 * time.Minute
)

type Syncer struct {
	store      *Store
	dataDir    string
	keyWrapper crypto.KeyWrapper
	encStore   *account.Store
	logger     *slog.Logger
	locks      sync.Map // accountID → *sync.Mutex; serializes Run per account
}

func NewSyncer(store *Store, dataDir string, keyWrapper crypto.KeyWrapper, encStore *account.Store) *Syncer {
	return &Syncer{
		store:      store,
		dataDir:    dataDir,
		keyWrapper: keyWrapper,
		encStore:   encStore,
		logger:     slog.Default(),
	}
}

// markStaleRunsForAccount clears any "running" sync runs older than
// staleRunAge for a specific account. Called at startup and before each Run().
func (s *Syncer) markStaleRunsForAccount(ctx context.Context, accountID int64) error {
	runs, err := s.store.accountSt.ListRecentRuns(ctx, accountID)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, r := range runs {
		if r.Status != "running" || r.FinishedAt != nil {
			continue
		}
		started, err := time.Parse(time.DateTime, r.StartedAt)
		if err != nil {
			continue
		}
		if now.Sub(started) > staleRunAge {
			_, _ = s.store.accountSt.UpdateSyncRunStatus(ctx, r.ID, 0,
				"abandoned: ran longer than "+staleRunAge.String(),
				"failed", 0)
			s.logger.Info("marked stale running sync as failed", "run_id", r.ID)
		}
	}
	return nil
}

// MarkAllStaleRuns fails every "running" sync run across all accounts. Called
// once at startup: a sync cannot survive a process restart, so any run still
// marked "running" is orphaned and must be cleared unconditionally (not just
// by age) or the account stays blocked until the stale age elapses.
func (s *Syncer) MarkAllStaleRuns(ctx context.Context) error {
	accounts, err := s.store.accountSt.ListAccounts(ctx)
	if err != nil {
		return fmt.Errorf("list accounts for stale run cleanup: %w", err)
	}
	for _, a := range accounts {
		runs, err := s.store.accountSt.ListRecentRuns(ctx, a.Account.ID)
		if err != nil {
			s.logger.Warn("failed to list runs for stale cleanup", "account_id", a.Account.ID, "error", err)
			continue
		}
		for _, r := range runs {
			if r.Status != "running" {
				continue
			}
			if _, err := s.store.accountSt.UpdateSyncRunStatus(ctx, r.ID, r.EmailsBackedUp,
				"interrupted: process restarted mid-sync", "failed", 0); err != nil {
				s.logger.Warn("failed to clear orphaned run", "run_id", r.ID, "error", err)
				continue
			}
			s.logger.Info("cleared orphaned sync run at startup", "run_id", r.ID, "account_id", a.Account.ID)
		}
	}
	return nil
}

func (s *Syncer) Run(ctx context.Context, accountID int64) error {
	acct, err := s.store.accountSt.GetAccount(ctx, accountID)
	if err != nil {
		return fmt.Errorf("get account %d: %w", accountID, err)
	}

	// Serialize syncs per account. A lock held for the whole Run means a
	// concurrent trigger fails fast instead of queuing a duplicate, and a
	// slow account never blocks syncs of other accounts.
	lockAny, _ := s.locks.LoadOrStore(accountID, &sync.Mutex{})
	lock := lockAny.(*sync.Mutex)
	if !lock.TryLock() {
		return fmt.Errorf("sync already running for account %d", accountID)
	}
	defer lock.Unlock()

	// Clear stale "running" records (inside the lock so a live run in this
	// process can't be marked stale by a concurrent trigger).
	if err := s.markStaleRunsForAccount(ctx, accountID); err != nil {
		s.logger.Warn("failed to clear stale runs", "account_id", accountID, "error", err)
	}

	currentRun, err := s.store.GetCurrentSyncRun(ctx, accountID)
	if err != nil {
		return fmt.Errorf("check current sync run: %w", err)
	}
	if currentRun != nil {
		s.logger.Warn("sync already running for account", "account_id", accountID)
		return fmt.Errorf("sync already running for account %d", accountID)
	}

	syncRun, err := s.store.accountSt.CreateSyncRun(ctx, accountID)
	if err != nil {
		return fmt.Errorf("create sync run: %w", err)
	}

	s.logger.Info("starting sync run", "sync_run_id", syncRun.ID, "account_id", accountID, "account_name", acct.Name)

	// Initialize encryption metadata if this is a new account
	if s.keyWrapper != nil && s.encStore != nil {
		_, err := s.encStore.GetEncryption(ctx, accountID)
		if err != nil {
			// The wrapped content key is stored as encryptedCK, not the master key ID.
			// GetEncryptionByPrefix returns the full wrapped CK which we prefix-match.
			// We use the wrapper's fingerprint as a stable prefix.
			fingerprint := s.keyWrapper.Fingerprint()
			_, err := s.encStore.UpsertEncryption(ctx, accountID, s.keyWrapper.ID(), fingerprint, fingerprint)
			if err != nil {
				s.logger.Warn("failed to init encryption metadata", "account_id", accountID, "error", err)
			} else {
				s.logger.Info("encryption initialized for account", "account_id", accountID)
			}
		}
	}

	folders := acct.Folders
	if len(folders) == 0 {
		folders = []string{"INBOX"}
	}

	var errors []string
	var emailsBackedUp int64
	var lastUID int64

	for _, folder := range folders {
		folderLastUID, folderCount, folderErrs := s.syncFolder(ctx, acct, folder, syncRun.ID)
		if folderLastUID > lastUID {
			lastUID = folderLastUID
		}
		emailsBackedUp += folderCount
		errors = append(errors, folderErrs...)
		if len(errors) > maxErrors {
			errors = errors[:maxErrors]
		}

		// FIX #7: Store per-folder last UID so other folders with
		// lower UID spaces don't get skipped forever.
		if folderLastUID > 0 {
			if err := s.store.accountSt.UpsertFolderSyncState(ctx, acct.ID, folder, folderLastUID); err != nil {
				s.logger.Warn("failed to store folder sync state", "folder", folder, "error", err)
			}
		}
	}

	// Determine final status
	finalStatus := "completed"
	if len(errors) > 0 {
		finalStatus = "partial"
	}

	// Build error string
	var errorsStr string
	if len(errors) > 0 {
		errorsStr = strings.Join(errors, "; ")
		if len(errorsStr) > maxErrorsLen {
			errorsStr = errorsStr[:maxErrorsLen]
		}
	}

	_, err = s.store.accountSt.UpdateSyncRunStatus(ctx, syncRun.ID, emailsBackedUp, errorsStr, finalStatus, lastUID)
	if err != nil {
		s.logger.Error("failed to update sync run status", "sync_run_id", syncRun.ID, "error", err)
	}

	if len(errors) > 0 {
		return fmt.Errorf("sync completed with %d errors for account %d", len(errors), accountID)
	}

	s.logger.Info("sync completed", "sync_run_id", syncRun.ID, "account_id", accountID, "emails_backed_up", emailsBackedUp)
	return nil
}

func (s *Syncer) syncFolder(ctx context.Context, acct account.Account, folder string, syncRunID int64) (lastUID int64, count int64, errs []string) {
	c, err := s.connectIMAP(acct)
	if err != nil {
		errs = append(errs, fmt.Sprintf("folder %s: connect: %v", folder, err))
		return 0, 0, errs
	}
	defer c.Logout()

	// Select the folder
	_, err = c.Select(folder, false)
	if err != nil {
		errs = append(errs, fmt.Sprintf("folder %s: select: %v", folder, err))
		return 0, 0, errs
	}

	// Get last sync state for this specific folder.
	// For SSL accounts we use the account-level last UID; for STARTTLS
	// (which may have different folders), we use folder-level tracking.
	lastSyncUID, _ := s.store.GetLastSyncStateForFolder(ctx, acct.ID, folder)

	// Build UID search criteria — sync by UID range only, never UNSEEN.
	var criteria imap.SearchCriteria
	if lastSyncUID > 0 {
		// Search for all messages (including seen) after the last known UID.
		// This catches messages that were already seen or were not marked
		// unseen server-side (e.g., via IMAP APPEND).
		criteria.Uid = &imap.SeqSet{}
		criteria.Uid.AddRange(uint32(lastSyncUID)+1, ^uint32(0))
	} else {
		// First sync: fetch all messages in the folder.
		criteria.Uid = &imap.SeqSet{}
		criteria.Uid.AddRange(1, ^uint32(0))
	}

	// Search for messages by UID
	uids, err := c.UidSearch(&criteria)
	if err != nil {
		errs = append(errs, fmt.Sprintf("folder %s: search: %v", folder, err))
		return 0, 0, errs
	}

	if len(uids) == 0 {
		s.logger.Info("no messages to sync", "folder", folder, "account_id", acct.ID)
		return 0, 0, nil
	}

	total := len(uids)
	s.logger.Info("found messages to sync", "folder", folder, "account_id", acct.ID, "count", total)

	// go-imap v1.2.1's UidFetch loads every requested message into memory and
	// has no context. Fetching an entire large mailbox (tens of thousands of
	// full RFC822 bodies) in one call exhausts memory and cannot finish inside
	// any sane timeout. Fetch in bounded batches instead: memory stays capped
	// at one batch, each batch has its own timeout, and progress is persisted
	// after every batch so an interrupted sync resumes from a watermark
	// instead of restarting from zero.
	// BODY.PEEK[] fetches the full message WITHOUT setting the \Seen flag, so
	// backing up never changes read/unread state on the server. Plain RFC822
	// (== BODY[]) would mark every fetched message as read.
	bodySection := &imap.BodySectionName{Peek: true}
	fetchItems := []imap.FetchItem{imap.FetchEnvelope, imap.FetchFlags, imap.FetchInternalDate, bodySection.FetchItem()}

	// The watermark must only advance over UIDs that were written
	// successfully: track the highest success and the lowest failure.
	// safeWatermark is the highest watermark from a fully-drained batch — on
	// an aborted batch we return that rather than maxSuccess, so UIDs that
	// were never fetched are retried next run instead of being skipped.
	var maxSuccess, minFailed, safeWatermark int64

	for start := 0; start < total; start += fetchBatchSize {
		if ctx.Err() != nil {
			errs = appendErr(errs, fmt.Sprintf("folder %s: cancelled after %d/%d", folder, count, total))
			return safeWatermark, count, errs
		}

		end := min(start+fetchBatchSize, total)
		uidSet := new(imap.SeqSet)
		for _, uid := range uids[start:end] {
			uidSet.AddNum(uid)
		}

		// Buffer sized to the batch so UidFetch never blocks sending; the
		// library closes msgCh after UidFetch returns, so we must NOT close it.
		msgCh := make(chan *imap.Message, end-start)
		fetchDone := make(chan error, 1)
		go func() { fetchDone <- c.UidFetch(uidSet, fetchItems, msgCh) }()

		timer := time.NewTimer(fetchTimeout)
		aborted := ""
	drain:
		for {
			select {
			case msg, ok := <-msgCh:
				if !ok {
					break drain
				}
				if err := s.fetchAndWriteMessage(ctx, acct, folder, msg.Uid, msg); err != nil {
					errMsg := fmt.Sprintf("folder %s: UID %d: %v", folder, msg.Uid, err)
					s.logger.Error(errMsg)
					errs = appendErr(errs, errMsg)
					if minFailed == 0 || int64(msg.Uid) < minFailed {
						minFailed = int64(msg.Uid)
					}
					continue
				}
				count++
				if int64(msg.Uid) > maxSuccess {
					maxSuccess = int64(msg.Uid)
				}
			case <-timer.C:
				aborted = fmt.Sprintf("folder %s: fetch timed out after %v at %d/%d", folder, fetchTimeout, count, total)
				break drain
			case <-ctx.Done():
				aborted = fmt.Sprintf("folder %s: cancelled at %d/%d", folder, count, total)
				break drain
			}
		}
		timer.Stop()

		if aborted != "" {
			// Kill the connection so the in-flight UidFetch unblocks, then
			// wait for its goroutine before the deferred Logout runs.
			c.Close()
			<-fetchDone
			errs = appendErr(errs, aborted)
			return safeWatermark, count, errs
		}

		if err := <-fetchDone; err != nil {
			errs = appendErr(errs, fmt.Sprintf("folder %s: fetch batch %d-%d: %v", folder, start, end, err))
			return safeWatermark, count, errs
		}

		// Batch fully drained — persist progress so a later interruption
		// resumes here and the dashboard reflects the running total.
		safeWatermark = watermark(maxSuccess, minFailed)
		if safeWatermark > 0 {
			if err := s.store.accountSt.UpsertFolderSyncState(ctx, acct.ID, folder, safeWatermark); err != nil {
				s.logger.Warn("failed to persist folder sync state", "folder", folder, "error", err)
			}
		}
		if syncRunID > 0 {
			if err := s.store.accountSt.MarkSyncRunProgress(ctx, syncRunID, count, safeWatermark); err != nil {
				s.logger.Warn("failed to record sync progress", "sync_run_id", syncRunID, "error", err)
			}
		}
		s.logger.Info("sync progress", "folder", folder, "account_id", acct.ID, "done", count, "total", total)
	}

	return watermark(maxSuccess, minFailed), count, errs
}

// appendErr appends an error message while keeping the slice bounded so a
// mailbox full of individually-failing messages can't grow it without limit.
func appendErr(errs []string, msg string) []string {
	if len(errs) >= maxErrors {
		return errs
	}
	return append(errs, msg)
}

// watermark returns the highest UID below which every message succeeded.
func watermark(maxSuccess, minFailed int64) int64 {
	if minFailed > 0 && minFailed-1 < maxSuccess {
		return minFailed - 1
	}
	return maxSuccess
}

func (s *Syncer) connectIMAP(acct account.Account) (*client.Client, error) {
	addr := fmt.Sprintf("%s:%d", acct.Host, acct.Port)

	var c *client.Client
	var err error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		if attempt > 1 {
			delay := retryBaseDelay * time.Duration(1<<(attempt-1))
			s.logger.Info("retrying IMAP connection", "attempt", attempt, "delay", delay, "account", acct.Name)
			time.Sleep(delay)
		}

		if acct.UseSsl {
			c, err = s.connectSSL(addr, acct)
		} else {
			c, err = s.connectSTARTTLS(addr, acct)
		}

		if err == nil {
			return c, nil
		}

		s.logger.Warn("IMAP connection failed", "attempt", attempt, "account", acct.Name, "error", err)
	}

	return nil, fmt.Errorf("failed to connect to %s after %d attempts: %w", addr, maxRetries, err)
}

// loginPassword returns the plaintext IMAP password for an account.
// With a master key the stored value is AES-GCM ciphertext; without one
// it is stored as plaintext.
func (s *Syncer) loginPassword(acct account.Account) (string, error) {
	if !s.store.accountSt.MasterKeySet() {
		return acct.EncryptedPassword, nil
	}
	plaintext, err := s.store.accountSt.DecryptPassword(acct.EncryptedPassword)
	if err != nil {
		return "", fmt.Errorf("decrypt password: %w", err)
	}
	return plaintext, nil
}

func (s *Syncer) connectSSL(addr string, acct account.Account) (*client.Client, error) {
	c, err := client.DialTLS(addr, &tls.Config{
		ServerName: acct.Host,
	})
	if err != nil {
		return nil, fmt.Errorf("dial TLS: %w", err)
	}

	password, err := s.loginPassword(acct)
	if err != nil {
		c.Close()
		return nil, err
	}

	if err := c.Login(acct.Username, password); err != nil {
		c.Close()
		return nil, fmt.Errorf("login: %w", err)
	}

	return c, nil
}

func (s *Syncer) connectSTARTTLS(addr string, acct account.Account) (*client.Client, error) {
	dialer := &net.Dialer{
		Timeout: connectionTimeout,
	}

	conn, err := dialer.DialContext(context.Background(), "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}

	// FIX #3: Pass ServerName so TLS validation works.
	// Without it, StartTLS(nil) uses an empty ServerName and Go's
	// crypto/tls rejects it with "either ServerName or InsecureSkipVerify".
	c, err := client.New(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("new client: %w", err)
	}

	if err := c.StartTLS(&tls.Config{
		ServerName: acct.Host,
	}); err != nil {
		c.Close()
		return nil, fmt.Errorf("start TLS: %w", err)
	}

	password, err := s.loginPassword(acct)
	if err != nil {
		c.Close()
		return nil, err
	}

	if err := c.Login(acct.Username, password); err != nil {
		c.Close()
		return nil, fmt.Errorf("login: %w", err)
	}

	return c, nil
}

func (s *Syncer) fetchAndWriteMessage(ctx context.Context, acct account.Account, folder string, uid uint32, msg *imap.Message) error {
	// Check if .eml file already exists (dedup)
	date := time.Unix(msg.InternalDate.Unix(), 0).UTC().Format("2006-01-02")
	emlPath := s.emlPath(acct.ID, folder, date, uid)

	if _, err := os.Stat(emlPath); err == nil {
		s.logger.Debug("skipping existing eml file", "path", emlPath, "uid", uid)
		return nil
	}

	// Get RFC822 body from the message
	var rawBody []byte
	for _, section := range msg.Body {
		data, err := io.ReadAll(section)
		if err != nil {
			return fmt.Errorf("read body: %w", err)
		}
		rawBody = data
	}

	if len(rawBody) == 0 {
		return fmt.Errorf("empty message body for UID %d", uid)
	}

	return s.writeEML(emlPath, rawBody)
}

func (s *Syncer) emlPath(accountID int64, folder, date string, uid uint32) string {
	ext := ".eml"
	if s.keyWrapper != nil {
		ext = ".eml.enc"
	}
	return filepath.Join(s.dataDir, strconv.FormatInt(accountID, 10), folder, fmt.Sprintf("%s_%d%s", date, uid, ext))
}

func (s *Syncer) writeEML(path string, data []byte) error {
	dir := filepath.Dir(path)

	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}

	if s.keyWrapper != nil {
		return crypto.EncryptFile(path, data, s.keyWrapper)
	}

	// Write to temp file first, then rename (atomic write).
	// FIX (minor): fsync before close so data hits disk.
	tmpFile, err := os.CreateTemp(dir, ".eml.tmp.*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmpFile.Name()

	_, err = tmpFile.Write(data)
	if err != nil {
		tmpFile.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write temp file: %w", err)
	}

	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		os.Remove(tmpName)
		return fmt.Errorf("sync temp file: %w", err)
	}

	if err := tmpFile.Chmod(0600); err != nil {
		tmpFile.Close()
		os.Remove(tmpName)
		return fmt.Errorf("set permissions: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp file: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename to final path: %w", err)
	}

	return nil
}

func (s *Syncer) SyncAll(ctx context.Context) {
	accounts, err := s.store.accountSt.ListAccounts(ctx)
	if err != nil {
		s.logger.Error("failed to list accounts for sync", "error", err)
		return
	}

	for _, acctWithSync := range accounts {
		acct := acctWithSync.Account

		if acctWithSync.IsSyncing {
			s.logger.Debug("skipping account, sync already running", "account_id", acct.ID)
			continue
		}

		if err := s.Run(ctx, acct.ID); err != nil {
			s.logger.Error("sync failed for account", "account_id", acct.ID, "error", err)
		}
	}
}
