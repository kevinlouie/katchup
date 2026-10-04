package imap

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
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
	// commandTimeout bounds every IMAP command except the body fetch (which has
	// its own timer in syncFolder), so a silently dead connection fails the
	// folder instead of wedging the run — and the per-account lock — forever.
	commandTimeout = 2 * time.Minute
	fetchTimeout   = 120 * time.Second
	// maxRunDuration caps one sync run. Progress is persisted per batch, so a
	// run cut off here resumes from its watermark on the next sync.
	maxRunDuration = 6 * time.Hour
	fetchBatchSize = 200
	maxRetries     = 3
	retryBaseDelay = 1 * time.Second
	maxErrors      = 5
	maxErrorsLen   = 1000
	staleRunAge    = 30 * time.Minute
)

type Syncer struct {
	store      *Store
	dataDir    string
	keyWrapper crypto.KeyWrapper
	encStore   *account.Store
	logger     *slog.Logger
	locks      sync.Map // accountID → *sync.Mutex; serializes Run per account

	// ThrottleCooldown is how long an account is skipped after the provider signals
	// throttling. Defaults to 24h in NewSyncer; set to 0 to disable backoff.
	ThrottleCooldown time.Duration
	// FetchPacing is an optional delay between fetch batches (default 0 = none).
	FetchPacing time.Duration

	// runBody performs the actual folder-by-folder backup for an already-created
	// run while the per-account lock is held. It is a field (defaulting to
	// executeSync) so tests can stub the real IMAP work when exercising the
	// trigger/coalesce coordination without a live IMAP server.
	runBody func(ctx context.Context, acct account.Account, syncRun account.SyncRun) error
}

func NewSyncer(store *Store, dataDir string, keyWrapper crypto.KeyWrapper, encStore *account.Store) *Syncer {
	s := &Syncer{
		store:      store,
		dataDir:    dataDir,
		keyWrapper: keyWrapper,
		encStore:   encStore,
		logger:     slog.Default(),

		ThrottleCooldown: 24 * time.Hour,
	}
	s.runBody = s.executeSync
	return s
}

// throttleMarkers are substrings (matched case-insensitively) that a provider
// returns when rate-limiting or bandwidth-capping IMAP. They are deliberately
// specific so a transient/unrelated error is not misread as a throttle (which
// would suppress backups for the whole cooldown).
var throttleMarkers = []string{
	"bandwidth",
	"over quota",
	"overquota",
	"too many simultaneous",
	"too many connections",
	"too many login",
	"too many messages",
}

// isThrottleError reports whether an error message looks like provider throttling.
func isThrottleError(msg string) bool {
	m := strings.ToLower(msg)
	for _, k := range throttleMarkers {
		if strings.Contains(m, k) {
			return true
		}
	}
	return false
}

// containsThrottle reports whether any error in the slice looks like throttling.
func containsThrottle(errs []string) bool {
	for _, e := range errs {
		if isThrottleError(e) {
			return true
		}
	}
	return false
}

// throttledUntil reports whether an account is in a throttle cooldown, based on
// its most recent run being marked "throttled", and until when. Disabled when
// ThrottleCooldown <= 0.
func (s *Syncer) throttledUntil(ctx context.Context, accountID int64) (time.Time, bool) {
	if s.ThrottleCooldown <= 0 {
		return time.Time{}, false
	}
	runs, err := s.store.accountSt.ListRecentRuns(ctx, accountID)
	if err != nil || len(runs) == 0 {
		return time.Time{}, false
	}
	last := runs[0] // ordered started_at DESC, id DESC
	if last.Status != "throttled" || last.FinishedAt == nil {
		return time.Time{}, false
	}
	finished, err := time.Parse(time.DateTime, *last.FinishedAt)
	if err != nil {
		return time.Time{}, false
	}
	until := finished.Add(s.ThrottleCooldown)
	if time.Since(finished) < s.ThrottleCooldown {
		return until, true
	}
	return time.Time{}, false
}

// lockFor returns the per-account execution mutex, creating it on first use. The
// lock is held for the entire duration of a sync so a concurrent Run or trigger
// fails fast (TryLock) instead of starting a duplicate.
func (s *Syncer) lockFor(accountID int64) *sync.Mutex {
	lockAny, _ := s.locks.LoadOrStore(accountID, &sync.Mutex{})
	return lockAny.(*sync.Mutex)
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

// Run performs a full synchronous sync of an account, holding the per-account
// lock for the whole run. A concurrent Run or trigger for the same account fails
// fast (TryLock) instead of starting a duplicate.
func (s *Syncer) Run(ctx context.Context, accountID int64) error {
	lock := s.lockFor(accountID)
	if !lock.TryLock() {
		return fmt.Errorf("sync already running for account %d", accountID)
	}
	defer lock.Unlock()

	acct, syncRun, err := s.beginRun(ctx, accountID)
	if err != nil {
		return err
	}
	if syncRun == nil {
		s.logger.Warn("sync already running for account", "account_id", accountID)
		return fmt.Errorf("sync already running for account %d", accountID)
	}
	return s.runBody(ctx, acct, *syncRun)
}

// beginRun prepares a new sync run for an account. The caller MUST hold the
// per-account lock. It clears stale "running" records, verifies no run is already
// in flight per the sync_runs table, and creates a fresh run row. A nil *SyncRun
// with a nil error means a run is already in flight (per the DB) and the caller
// must not proceed.
func (s *Syncer) beginRun(ctx context.Context, accountID int64) (account.Account, *account.SyncRun, error) {
	acct, err := s.store.accountSt.GetAccount(ctx, accountID)
	if err != nil {
		return account.Account{}, nil, fmt.Errorf("get account %d: %w", accountID, err)
	}

	// Clear stale "running" records (inside the lock so a live run in this
	// process can't be marked stale by a concurrent trigger).
	if err := s.markStaleRunsForAccount(ctx, accountID); err != nil {
		s.logger.Warn("failed to clear stale runs", "account_id", accountID, "error", err)
	}

	currentRun, err := s.store.GetCurrentSyncRun(ctx, accountID)
	if err != nil {
		return account.Account{}, nil, fmt.Errorf("check current sync run: %w", err)
	}
	if currentRun != nil {
		return acct, nil, nil
	}

	syncRun, err := s.store.accountSt.CreateSyncRun(ctx, accountID)
	if err != nil {
		return account.Account{}, nil, fmt.Errorf("create sync run: %w", err)
	}
	return acct, &syncRun, nil
}

// TriggerSync is the on-demand entry point (POST /api/sync). It coalesces
// requests so at most one sync runs per account: if a sync is already in flight,
// or one finished within coalesceWindow, the existing run's id is returned and no
// new sync starts. Otherwise a run row is created synchronously (so its id can be
// returned immediately) and executed in a background goroutine that releases the
// per-account lock when done. The bool reports whether a new sync was started.
func (s *Syncer) TriggerSync(ctx context.Context, accountID int64, coalesceWindow time.Duration) (runID int64, started bool, err error) {
	// Respect an active throttle cooldown: don't let an on-demand trigger poke a
	// provider that just rate-limited us. Return the throttled run id, not started.
	if until, yes := s.throttledUntil(ctx, accountID); yes {
		s.logger.Warn("sync trigger ignored, provider throttled — backing off",
			"account_id", accountID, "until", until.UTC().Format(time.RFC3339))
		if runs, lerr := s.store.accountSt.ListRecentRuns(ctx, accountID); lerr == nil && len(runs) > 0 {
			return runs[0].ID, false, nil
		}
		return 0, false, nil
	}

	lock := s.lockFor(accountID)
	if !lock.TryLock() {
		// A sync is in flight (the lock is held for the whole run). Join it by
		// returning the running run's id. The run row may not be visible for a
		// brief window after the lock is taken, so retry shortly.
		if run := s.awaitRunningRun(ctx, accountID); run != nil {
			return run.ID, false, nil
		}
		return 0, false, nil
	}

	// We hold the lock: nothing is executing for this account in-process. Since
	// run creation always happens under this lock, no other path can start a run
	// concurrently — so the coalesce decision below is race-free.

	// Coalesce with a run that finished within the window.
	if id, ok, cerr := s.recentRunWithinWindow(ctx, accountID, coalesceWindow); cerr != nil {
		lock.Unlock()
		return 0, false, cerr
	} else if ok {
		lock.Unlock()
		return id, false, nil
	}

	acct, syncRun, berr := s.beginRun(ctx, accountID)
	if berr != nil {
		lock.Unlock()
		return 0, false, berr
	}
	if syncRun == nil {
		// The DB reports a run in flight (e.g. a recent orphaned "running" row not
		// yet past the stale age) even though no in-process sync holds the lock.
		// Coalesce onto it rather than starting another.
		lock.Unlock()
		if run := s.awaitRunningRun(ctx, accountID); run != nil {
			return run.ID, false, nil
		}
		return 0, false, nil
	}

	// Execute asynchronously. The goroutine owns the lock from here and releases
	// it when the sync completes. context.Background() is used (not the request
	// context) so the sync is not cancelled when the HTTP response is written.
	run := *syncRun
	go func() {
		defer lock.Unlock()
		if err := s.runBody(context.Background(), acct, run); err != nil {
			s.logger.Error("triggered sync failed", "account_id", accountID, "sync_run_id", run.ID, "error", err)
		}
	}()

	return run.ID, true, nil
}

// awaitRunningRun polls briefly for a "running" sync run to appear, bridging the
// short window between a run starting and its row being visible to another
// trigger that lost the lock race.
func (s *Syncer) awaitRunningRun(ctx context.Context, accountID int64) *account.SyncRun {
	for i := 0; i < 50; i++ {
		run, err := s.store.GetCurrentSyncRun(ctx, accountID)
		if err == nil && run != nil {
			return run
		}
		time.Sleep(2 * time.Millisecond)
	}
	return nil
}

// recentRunWithinWindow reports the id of the most recent finished run when it
// completed less than window ago, so an on-demand trigger can coalesce onto it
// instead of re-syncing. A non-positive window disables coalescing.
func (s *Syncer) recentRunWithinWindow(ctx context.Context, accountID int64, window time.Duration) (int64, bool, error) {
	if window <= 0 {
		return 0, false, nil
	}
	runs, err := s.store.accountSt.ListRecentRuns(ctx, accountID)
	if err != nil {
		return 0, false, err
	}
	if len(runs) == 0 {
		return 0, false, nil
	}
	last := runs[0] // ListRecentRuns is ordered started_at DESC, id DESC
	if last.FinishedAt == nil {
		return 0, false, nil
	}
	finished, err := time.Parse(time.DateTime, *last.FinishedAt)
	if err != nil {
		return 0, false, nil
	}
	if time.Since(finished) < window {
		return last.ID, true, nil
	}
	return 0, false, nil
}

// executeSync runs the folder-by-folder backup for a created run and records the
// final status. The caller MUST hold the per-account lock and have created the
// run via beginRun.
func (s *Syncer) executeSync(ctx context.Context, acct account.Account, syncRun account.SyncRun) error {
	accountID := acct.ID

	ctx, cancel := context.WithTimeout(ctx, maxRunDuration)
	defer cancel()

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
	throttled := false

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

		// Store per-folder last UID so other folders with
		// lower UID spaces don't get skipped forever.
		if folderLastUID > 0 {
			if err := s.store.accountSt.UpsertFolderSyncState(ctx, acct.ID, folder, folderLastUID); err != nil {
				s.logger.Warn("failed to store folder sync state", "folder", folder, "error", err)
			}
		}

		// Provider throttling: stop hitting it immediately and let the account
		// cool down (the next sync is skipped until ThrottleCooldown elapses)
		// rather than hammering the remaining folders into a longer ban.
		if containsThrottle(folderErrs) {
			throttled = true
			s.logger.Warn("provider throttling detected — backing off",
				"account_id", acct.ID, "folder", folder, "cooldown", s.ThrottleCooldown)
			break
		}
	}

	// Determine final status. "throttled" is distinct from "partial" so the next
	// sync can recognise the cooldown and skip instead of retrying into the ban.
	finalStatus := "completed"
	switch {
	case throttled:
		finalStatus = "throttled"
	case len(errors) > 0:
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

	_, err := s.store.accountSt.UpdateSyncRunStatus(ctx, syncRun.ID, emailsBackedUp, errorsStr, finalStatus, lastUID)
	if err != nil {
		s.logger.Error("failed to update sync run status", "sync_run_id", syncRun.ID, "error", err)
	}

	// A throttle is an expected backoff, not a failure — the run is recorded as
	// "throttled" and the account cools down. Don't surface it as an error.
	if throttled {
		s.logger.Warn("sync backed off (throttled)", "sync_run_id", syncRun.ID, "account_id", accountID, "emails_backed_up", emailsBackedUp)
		return nil
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
	mbox, err := c.Select(folder, false)
	if err != nil {
		errs = append(errs, fmt.Sprintf("folder %s: select: %v", folder, err))
		return 0, 0, errs
	}
	uidValidity := int64(mbox.UidValidity)

	// Resume from this folder's watermark — unless the server renumbered the
	// folder since it was recorded, in which case start over.
	lastSyncUID, err := s.reconcileUIDValidity(ctx, acct.ID, folder, uidValidity)
	if err != nil {
		errs = append(errs, fmt.Sprintf("folder %s: sync state: %v", folder, err))
		return 0, 0, errs
	}

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
		// The drain loop's timer bounds the fetch; c.Timeout would put a
		// deadline on the whole batch transfer instead.
		c.Timeout = 0
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
				if err := s.fetchAndWriteMessage(ctx, acct, folder, uidValidity, msg.Uid, msg); err != nil {
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

		err := <-fetchDone
		c.Timeout = commandTimeout
		if err != nil {
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

		// Optional pacing between batches to be gentler on provider rate limits.
		if s.FetchPacing > 0 {
			select {
			case <-ctx.Done():
				return safeWatermark, count, errs
			case <-time.After(s.FetchPacing):
			}
		}
	}

	return watermark(maxSuccess, minFailed), count, errs
}

// reconcileUIDValidity compares a folder's UIDVALIDITY on the server with the
// one recorded alongside its watermark and returns the UID to resume after:
//   - first time seen (recorded 0): adopt it, keep the watermark, and stamp the
//     folder's pre-tracking messages with it so they still count as archived;
//   - unchanged: the stored watermark;
//   - changed: the server renumbered the folder, so every recorded UID is
//     meaningless — reset the watermark to 0 and re-scan. Archived messages are
//     kept under their old generation; content dedup avoids rewriting blobs.
//
// A server reporting no UIDVALIDITY (0) leaves the old behaviour untouched.
func (s *Syncer) reconcileUIDValidity(ctx context.Context, accountID int64, folder string, uidValidity int64) (int64, error) {
	lastUID, stored, err := s.store.accountSt.GetFolderSyncState(ctx, accountID, folder)
	if err != nil {
		return 0, err
	}
	switch {
	case uidValidity == 0 || stored == uidValidity:
		return lastUID, nil
	case stored == 0:
		if err := s.store.AdoptUIDValidity(ctx, accountID, folder, uidValidity); err != nil {
			return 0, err
		}
		if err := s.store.accountSt.SetFolderSyncState(ctx, accountID, folder, uidValidity, lastUID); err != nil {
			return 0, err
		}
		return lastUID, nil
	default:
		s.logger.Warn("folder UIDVALIDITY changed — server renumbered it, re-scanning from UID 1",
			"account_id", accountID, "folder", folder, "old", stored, "new", uidValidity)
		if err := s.store.accountSt.SetFolderSyncState(ctx, accountID, folder, uidValidity, 0); err != nil {
			return 0, err
		}
		return 0, nil
	}
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
	// The dialer timeout also bounds the TLS handshake and server greeting.
	dialer := &net.Dialer{Timeout: connectionTimeout}
	c, err := client.DialWithDialerTLS(dialer, addr, &tls.Config{
		ServerName: acct.Host,
	})
	if err != nil {
		return nil, fmt.Errorf("dial TLS: %w", err)
	}
	c.Timeout = commandTimeout

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
	// The dialer timeout also bounds the server greeting.
	dialer := &net.Dialer{
		Timeout: connectionTimeout,
	}

	c, err := client.DialWithDialer(dialer, addr)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	c.Timeout = commandTimeout

	// Pass ServerName so TLS validation works.
	// Without it, StartTLS(nil) uses an empty ServerName and Go's
	// crypto/tls rejects it with "either ServerName or InsecureSkipVerify".

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

func (s *Syncer) fetchAndWriteMessage(ctx context.Context, acct account.Account, folder string, uidValidity int64, uid uint32, msg *imap.Message) error {
	// Message-level idempotency: an aborted batch is retried next run, so a
	// message already indexed for (account, folder, uidvalidity, uid) must be
	// skipped — otherwise it would bump a blob refcount a second time.
	if exists, err := s.store.MessageExists(ctx, acct.ID, folder, uidValidity, int64(uid)); err == nil && exists {
		s.logger.Debug("skipping already-indexed message", "folder", folder, "uid", uid)
		return nil
	}

	// Get RFC822 body from the message.
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

	// Content hash over the raw RFC822 bytes is the per-account dedup key.
	sum := sha256.Sum256(rawBody)
	sha := hex.EncodeToString(sum[:])
	size := int64(len(rawBody))

	date := time.Unix(msg.InternalDate.Unix(), 0).UTC().Format("2006-01-02")
	relPath := s.relEmlPath(acct.ID, folder, date, uidValidity, uid)

	// If a blob with this content already exists for the account, reuse it and
	// bump the refcount — do NOT rewrite the file. Otherwise encrypt+write the
	// file and insert the blob.
	blob, found, err := s.store.GetBlobBySha(ctx, acct.ID, sha)
	if err != nil {
		return err
	}
	var blobID int64
	if found {
		blobID, err = s.store.UpsertBlob(ctx, acct.ID, sha, blob.Path, blob.Size)
		if err != nil {
			return err
		}
		s.logger.Debug("dedup: reused existing blob", "folder", folder, "uid", uid, "blob_id", blobID)
	} else {
		if err := s.writeEML(filepath.Join(s.dataDir, relPath), rawBody); err != nil {
			return err
		}
		blobID, err = s.store.UpsertBlob(ctx, acct.ID, sha, relPath, size)
		if err != nil {
			return err
		}
	}

	// Parse the header fields we index. The envelope is fetched alongside the
	// body (FetchEnvelope), so no separate RFC5322 parse is needed.
	var msgID, fromAddr, toAddr, subject string
	if msg.Envelope != nil {
		msgID = msg.Envelope.MessageId
		subject = msg.Envelope.Subject
		fromAddr = joinAddresses(msg.Envelope.From)
		toAddr = joinAddresses(msg.Envelope.To)
	}
	internalDate := msg.InternalDate.UTC().Format(time.RFC3339)

	return s.store.InsertAndIndexMessage(ctx, InsertMessageParams{
		AccountID:    acct.ID,
		Folder:       folder,
		UIDValidity:  uidValidity,
		UID:          int64(uid),
		BlobID:       blobID,
		MessageIDHdr: msgID,
		FuzzyFP:      fuzzyFingerprint(fromAddr, internalDate, subject),
		FromAddr:     fromAddr,
		ToAddr:       toAddr,
		Subject:      subject,
		InternalDate: internalDate,
		Size:         size,
	})
}

// relEmlPath returns the .eml(.enc) path RELATIVE to the data dir. This is what
// is stored in blobs.path so the file can be relocated with the data dir. The
// name is <date>_<uidvalidity>_<uid> so a renumbered folder can never overwrite
// an archived file; with an unknown (0) UIDVALIDITY it is the legacy
// <date>_<uid>.
func (s *Syncer) relEmlPath(accountID int64, folder, date string, uidValidity int64, uid uint32) string {
	ext := ".eml"
	if s.keyWrapper != nil {
		ext = ".eml.enc"
	}
	name := fmt.Sprintf("%s_%d%s", date, uid, ext)
	if uidValidity != 0 {
		name = fmt.Sprintf("%s_%d_%d%s", date, uidValidity, uid, ext)
	}
	return filepath.Join(strconv.FormatInt(accountID, 10), folder, name)
}

// joinAddresses renders IMAP envelope addresses as a comma-separated list of
// "mailbox@host".
func joinAddresses(addrs []*imap.Address) string {
	var parts []string
	for _, a := range addrs {
		if a == nil {
			continue
		}
		parts = append(parts, a.Address())
	}
	return strings.Join(parts, ", ")
}

// fuzzyFingerprint is the Message-ID fallback correlation key:
// sha256(lower(from)|internal_date|lower(subject)). Deterministic for a given
// (from, date, subject).
func fuzzyFingerprint(from, internalDate, subject string) string {
	h := sha256.Sum256([]byte(strings.ToLower(from) + "|" + internalDate + "|" + strings.ToLower(subject)))
	return hex.EncodeToString(h[:])
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
	// fsync before close so data hits disk.
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

		if until, yes := s.throttledUntil(ctx, acct.ID); yes {
			s.logger.Warn("skipping account, provider throttled — backing off",
				"account_id", acct.ID, "until", until.UTC().Format(time.RFC3339))
			continue
		}

		if acctWithSync.IsSyncing {
			s.logger.Debug("skipping account, sync already running", "account_id", acct.ID)
			continue
		}

		if err := s.Run(ctx, acct.ID); err != nil {
			s.logger.Error("sync failed for account", "account_id", acct.ID, "error", err)
		}
	}
}
