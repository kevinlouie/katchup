package api

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"katchup/internal/account"
	"katchup/internal/imap"
	"katchup/internal/search"
	searchView "katchup/internal/view/search"
)

// searchLimit caps how many results either backend returns.
const searchLimit = 100

// SearchHandler serves GET /search — header-only full-text search. When a
// Meilisearch searcher is configured it queries Meili; otherwise (or if Meili
// errors) it degrades to a SQLite LIKE over subject/from. Bodies are never
// searched: only header fields are indexed/queried.
type SearchHandler struct {
	store     *account.Store
	imapStore *imap.Store
	// searcher is the Meili backend, or nil when MEILI_URL is unset (DB fallback).
	searcher search.Searcher
}

func NewSearchHandler(store *account.Store, imapStore *imap.Store, searcher search.Searcher) *SearchHandler {
	return &SearchHandler{store: store, imapStore: imapStore, searcher: searcher}
}

func (h *SearchHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h.Search(w, r)
}

func (h *SearchHandler) Search(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()

	query := strings.TrimSpace(r.URL.Query().Get("q"))
	accountIDStr := r.URL.Query().Get("account")
	var accountID int64
	if accountIDStr != "" {
		accountID, _ = strconv.ParseInt(accountIDStr, 10, 64)
	}

	accounts, err := h.store.ListAccounts(ctx)
	if err != nil {
		slog.Error("failed to list accounts for search", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	accountViews := make([]searchView.AccountView, 0, len(accounts))
	for _, a := range accounts {
		accountViews = append(accountViews, searchView.AccountView{ID: a.Account.ID, Name: a.Account.Name})
	}

	data := searchView.PageData{
		Title:     "Search",
		Query:     query,
		AccountID: accountID,
		Accounts:  accountViews,
	}

	if query != "" {
		data.Searched = true
		results, backend := h.runSearch(ctx, query, accountID)
		data.Backend = backend
		data.Results = make([]searchView.ResultView, 0, len(results))
		for _, res := range results {
			data.Results = append(data.Results, searchView.ResultView{
				ID:      res.ID,
				Date:    res.Date,
				From:    res.From,
				Subject: res.Subject,
				Folder:  res.Folder,
			})
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := searchView.SearchPage(data).Render(ctx, w); err != nil {
		slog.Error("failed to render search page", "error", err)
	}
}

// runSearch queries Meili when configured, falling back to the SQLite LIKE query
// if Meili is unset or errors. It returns the results and a label naming the
// backend that served them.
func (h *SearchHandler) runSearch(ctx context.Context, query string, accountID int64) ([]search.Result, string) {
	if h.searcher != nil {
		results, err := h.searcher.Search(ctx, query, accountID, searchLimit)
		if err == nil {
			return results, "meilisearch"
		}
		slog.Warn("meili search failed, falling back to database", "error", err)
	}
	results, err := h.imapStore.SearchMessagesLike(ctx, accountID, query, searchLimit)
	if err != nil {
		slog.Error("database search failed", "error", err)
		return nil, "database"
	}
	return results, "database"
}
