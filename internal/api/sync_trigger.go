package api

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"katchup/internal/account"
	"katchup/internal/imap"
)

// SyncTriggerHandler serves the machine-facing POST /api/sync trigger. A caller
// pokes katchup to sync a mailbox before polling; the handler is safe against
// stampede (per-account mutex + coalesce window) and never blocks on the sync.
type SyncTriggerHandler struct {
	store          *account.Store
	syncer         *imap.Syncer
	coalesceWindow time.Duration
}

func NewSyncTriggerHandler(store *account.Store, syncer *imap.Syncer, coalesceWindow time.Duration) *SyncTriggerHandler {
	return &SyncTriggerHandler{
		store:          store,
		syncer:         syncer,
		coalesceWindow: coalesceWindow,
	}
}

// syncTriggerResponse is the 202 body returned to the caller.
type syncTriggerResponse struct {
	RunID     int64 `json:"run_id"`
	AccountID int64 `json:"account_id"`
	// Started is true when this request kicked off a new sync, false when it was
	// coalesced onto an in-flight or recently-finished run.
	Started bool `json:"started"`
}

// Trigger handles POST /api/sync?account=<id>. It returns 202 immediately with
// the run id (a new run when one was started, or the coalesced run otherwise) and
// runs the sync in the background.
func (h *SyncTriggerHandler) Trigger(w http.ResponseWriter, r *http.Request) {
	accountID, err := strconv.ParseInt(r.URL.Query().Get("account"), 10, 64)
	if err != nil || accountID <= 0 {
		writeJSONError(w, http.StatusBadRequest, "account query param required")
		return
	}

	ctx := r.Context()
	if _, err := h.store.GetAccount(ctx, accountID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSONError(w, http.StatusNotFound, "account not found")
			return
		}
		slog.Error("failed to get account for sync trigger", "account_id", accountID, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	runID, started, err := h.syncer.TriggerSync(ctx, accountID, h.coalesceWindow)
	if err != nil {
		slog.Error("sync trigger failed", "account_id", accountID, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	writeJSON(w, http.StatusAccepted, syncTriggerResponse{
		RunID:     runID,
		AccountID: accountID,
		Started:   started,
	})
}

// ServeHTTP dispatches the sync-trigger route.
func (h *SyncTriggerHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/sync" && r.Method == http.MethodPost {
		h.Trigger(w, r)
		return
	}
	writeJSONError(w, http.StatusNotFound, "not found")
}
