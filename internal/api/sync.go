package api

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"katchup/internal/account"
	syncView "katchup/internal/view/sync"

	"katchup/internal/imap"
)

type SyncHandler struct {
	store  *account.Store
	syncer *imap.Syncer
}

func NewSyncHandler(store *account.Store, syncer *imap.Syncer) *SyncHandler {
	return &SyncHandler{
		store:  store,
		syncer: syncer,
	}
}

func (h *SyncHandler) Status(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()

	accounts, err := h.store.ListAccounts(ctx)
	if err != nil {
		slog.Error("failed to list accounts for sync status", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	var views []syncView.AccountSyncView
	for _, a := range accounts {
		runs, _ := h.store.ListRecentRuns(ctx, a.Account.ID)

		var recentRuns []syncView.SyncRunView
		for _, run := range runs {
			srv := syncView.SyncRunView{
				ID:             run.ID,
				StartedAt:      run.StartedAt,
				EmailsBackedUp: run.EmailsBackedUp,
				Status:         run.Status,
			}
			if run.FinishedAt != nil {
				srv.FinishedAt = *run.FinishedAt
			}
			if run.LastUid != nil {
				srv.LastUID = strconv.FormatInt(*run.LastUid, 10)
			}
			if run.Errors != nil && *run.Errors != "" {
				srv.Errors = *run.Errors
			}
			recentRuns = append(recentRuns, srv)
		}

		lastSyncAt := ""
		lastUID := ""
		var emailsBackedUp int64
		var errors string

		if len(runs) > 0 {
			last := runs[0]
			if last.FinishedAt != nil {
				lastSyncAt = *last.FinishedAt
			}
			if last.LastUid != nil {
				lastUID = strconv.FormatInt(*last.LastUid, 10)
			}
			emailsBackedUp = last.EmailsBackedUp
			if last.Errors != nil {
				errors = *last.Errors
			}
		}

		foldersStr := ""
		for i, f := range a.Account.Folders {
			if i > 0 {
				foldersStr += ", "
			}
			foldersStr += f
		}

		views = append(views, syncView.AccountSyncView{
			ID:             a.Account.ID,
			Name:           a.Account.Name,
			Host:           a.Account.Host,
			Username:       a.Account.Username,
			Folders:        foldersStr,
			IsSyncing:      a.IsSyncing,
			LastSyncAt:      lastSyncAt,
			LastUID:        lastUID,
			EmailsBackedUp: emailsBackedUp,
			Errors:         errors,
			RecentRuns:     recentRuns,
		})
	}

	data := syncView.PageData{
		Title:    "Sync Status",
		Accounts: views,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := syncView.StatusPage(data).Render(ctx, w); err != nil {
		slog.Error("failed to render sync page", "error", err)
	}
}

func (h *SyncHandler) Trigger(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	accountID, err := parseAccountIDFromSyncPath(r.URL.Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// Verify account exists
	ctx := r.Context()
	_, err = h.store.GetAccount(ctx, accountID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		slog.Error("failed to get account for sync trigger", "id", accountID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// FIX #11: Use context.Background() instead of r.Context().
	// r.Context() is cancelled as soon as the HTTP response is written
	// (the redirect), so the background sync's DB calls would all fail.
	go func() {
		slog.Info("triggering manual sync", "account_id", accountID)
		if err := h.syncer.Run(context.Background(), accountID); err != nil {
			slog.Error("manual sync failed", "account_id", accountID, "error", err)
		}
	}()

	http.Redirect(w, r, "/sync", http.StatusSeeOther)
}

// parseAccountIDFromSyncPath extracts the account ID from /sync/{id}/trigger
func parseAccountIDFromSyncPath(path string) (int64, error) {
	path = strings.TrimPrefix(path, "/sync/")
	path = strings.TrimSuffix(path, "/trigger")

	idStr := path
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// ServeHTTP implements http.Handler for the sync handler.
func (h *SyncHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	switch {
	case path == "/sync" && r.Method == http.MethodGet:
		h.Status(w, r)
	case strings.HasSuffix(path, "/trigger") && r.Method == http.MethodPost:
		h.Trigger(w, r)
	default:
		http.NotFound(w, r)
	}
}
