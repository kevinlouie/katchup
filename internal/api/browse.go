package api

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"katchup/internal/account"
	"katchup/internal/crypto"
	"katchup/internal/imap"
	browseView "katchup/internal/view/browse"
)

type BrowseHandler struct {
	store      *account.Store
	imapStore  *imap.Store
	dataDir    string
	keyWrapper crypto.KeyWrapper
}

func NewBrowseHandler(store *account.Store, imapStore *imap.Store, dataDir string, keyWrapper crypto.KeyWrapper) *BrowseHandler {
	return &BrowseHandler{
		store:      store,
		imapStore:  imapStore,
		dataDir:    dataDir,
		keyWrapper: keyWrapper,
	}
}

func (h *BrowseHandler) List(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()

	accounts, err := h.store.ListAccounts(ctx)
	if err != nil {
		slog.Error("failed to list accounts for browse", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	var accountViews []browseView.AccountView
	for _, a := range accounts {
		count, err := h.imapStore.CountMessagesByAccount(ctx, a.Account.ID)
		if err != nil {
			slog.Error("failed to count messages for account", "account_id", a.Account.ID, "error", err)
		}
		accountViews = append(accountViews, browseView.AccountView{
			ID:     a.Account.ID,
			Name:   a.Account.Name,
			Emails: count,
		})
	}

	accountIDStr := r.URL.Query().Get("account")
	dateStr := r.URL.Query().Get("date")
	pageStr := r.URL.Query().Get("page")

	var accountID int64
	if accountIDStr != "" {
		accountID, _ = strconv.ParseInt(accountIDStr, 10, 64)
	}

	page := 1
	if pageStr != "" {
		page, _ = strconv.Atoi(pageStr)
		if page < 1 {
			page = 1
		}
	}

	perPage := 50
	filter := imap.MessageFilter{AccountID: accountID, Date: dateStr}

	total, err := h.imapStore.CountMessages(ctx, filter)
	if err != nil {
		slog.Error("failed to count messages", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	offset := int64((page - 1) * perPage)
	if offset > total {
		offset = total
	}

	msgs, err := h.imapStore.ListMessages(ctx, filter, int64(perPage), offset)
	if err != nil {
		slog.Error("failed to list messages", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	pageEmails := make([]browseView.EmailView, 0, len(msgs))
	for _, m := range msgs {
		date := m.InternalDate
		if len(date) >= 10 {
			date = date[:10]
		}
		pageEmails = append(pageEmails, browseView.EmailView{
			ID:        m.ID,
			Date:      date,
			From:      m.FromAddr,
			Subject:   m.Subject,
			Folder:    m.Folder,
			Size:      m.Size,
			AccountID: m.AccountID,
		})
	}

	data := browseView.PageData{
		Title:     "Browse Emails",
		Accounts:  accountViews,
		Emails:    pageEmails,
		AccountID: accountID,
		Date:      dateStr,
		Page:      page,
		PerPage:   perPage,
		Total:     int(total),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := browseView.BrowsePage(data).Render(ctx, w); err != nil {
		slog.Error("failed to render browse page", "error", err)
	}
}

// ServeHTTP implements http.Handler for the browse handler. Only the list at
// /browse is dispatched here; downloads are wired to Download directly by
// message id (/browse/download/{id}).
func (h *BrowseHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/browse" && r.Method == http.MethodGet {
		h.List(w, r)
		return
	}
	http.NotFound(w, r)
}

// parseEMLFilename splits a stored eml filename ("YYYY-MM-DD_<uid>.eml[.enc]")
// into its date and uid parts. Retained as a helper for filename handling.
func parseEMLFilename(name string) (date string, uid string, ok bool) {
	name = strings.TrimSuffix(name, ".eml.enc")
	name = strings.TrimSuffix(name, ".eml")

	parts := strings.SplitN(name, "_", 2)
	if len(parts) != 2 {
		return "", "", false
	}

	date = parts[0]
	uid = parts[1]

	if len(date) != 10 {
		return "", "", false
	}

	return date, uid, true
}

// formatSize formats a byte count to a human-readable string.
func formatSize(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)

	switch {
	case bytes >= GB:
		return strconv.FormatFloat(float64(bytes)/float64(GB), 'f', 1, 64) + "GB"
	case bytes >= MB:
		return strconv.FormatFloat(float64(bytes)/float64(MB), 'f', 1, 64) + "MB"
	case bytes >= KB:
		return strconv.FormatFloat(float64(bytes)/float64(KB), 'f', 1, 64) + "KB"
	default:
		return strconv.FormatInt(bytes, 10) + "B"
	}
}

// Download serves a single archived message, resolved by message id. It looks up
// the message's blob, reads blob.path (relative to the data dir), decrypts if
// needed, and serves the plaintext .eml.
func (h *BrowseHandler) Download(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	idStr := r.PathValue("id")
	if idStr == "" {
		// Fallback: last path segment (e.g. /browse/download/{id}).
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) > 0 {
			idStr = parts[len(parts)-1]
		}
	}

	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid message id", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	msg, err := h.imapStore.GetMessageWithBlob(ctx, id)
	if err != nil {
		http.Error(w, "message not found", http.StatusNotFound)
		return
	}

	plaintext, err := imap.ReadMessageBlob(h.dataDir, msg.BlobPath, h.keyWrapper)
	switch {
	case errors.Is(err, imap.ErrInvalidBlobPath):
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	case errors.Is(err, fs.ErrNotExist):
		http.Error(w, "file not found", http.StatusNotFound)
		return
	case err != nil:
		slog.Error("failed to read message blob via browse", "path", msg.BlobPath, "error", err)
		http.Error(w, "cannot read message", http.StatusInternalServerError)
		return
	}

	sanitized := sanitizeFilename(filepath.Base(msg.BlobPath))
	sanitized = strings.TrimSuffix(sanitized, ".enc")
	sanitized = strings.TrimSuffix(sanitized, ".eml")
	if sanitized == "" {
		sanitized = fmt.Sprintf("message-%d", msg.ID)
	}
	w.Header().Set("Content-Type", "message/rfc822")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s.eml\"", sanitized))
	w.Header().Set("Content-Length", strconv.Itoa(len(plaintext)))
	w.WriteHeader(http.StatusOK)
	w.Write(plaintext)

	slog.Info("served email via browse", "message_id", msg.ID, "account_id", msg.AccountID, "folder", msg.Folder)
}
