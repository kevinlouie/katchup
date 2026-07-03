package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"katchup/internal/account"
	"katchup/internal/crypto"
	browseView "katchup/internal/view/browse"
)

type BrowseHandler struct {
	store      *account.Store
	dataDir    string
	keyWrapper crypto.KeyWrapper
}

func NewBrowseHandler(store *account.Store, dataDir string, keyWrapper crypto.KeyWrapper) *BrowseHandler {
	return &BrowseHandler{
		store:      store,
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
	totalEmails := make(map[int64]int64)

	for _, a := range accounts {
		count := h.countEmailsForAccount(a.Account.ID)
		totalEmails[a.Account.ID] = count
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

	allEmails := h.scanEmails(accountID, dateStr)

	sort.Slice(allEmails, func(i, j int) bool {
		if allEmails[i].Date != allEmails[j].Date {
			return allEmails[i].Date > allEmails[j].Date
		}
		return allEmails[i].Filename > allEmails[j].Filename
	})

	total := len(allEmails)

	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	pageEmails := allEmails[start:end]

	data := browseView.PageData{
		Title:     "Browse Emails",
		Accounts:  accountViews,
		Emails:    pageEmails,
		AccountID: accountID,
		Date:      dateStr,
		Page:      page,
		PerPage:   perPage,
		Total:     total,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := browseView.BrowsePage(data).Render(ctx, w); err != nil {
		slog.Error("failed to render browse page", "error", err)
	}
}

func (h *BrowseHandler) scanEmails(accountID int64, dateStr string) []browseView.EmailView {
	var results []browseView.EmailView

	baseDir := h.dataDir

	if err := filepath.Walk(baseDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}

		name := info.Name()
		if !strings.HasSuffix(name, ".eml") && !strings.HasSuffix(name, ".eml.enc") {
			return nil
		}

		// Path structure: dataDir/{account_id}/{folder}/{filename}
		dir := filepath.Dir(path)
		accountDir := filepath.Dir(dir)
		acctID, err := strconv.ParseInt(filepath.Base(accountDir), 10, 64)
		if err != nil {
			return nil
		}

		if accountID != 0 && acctID != accountID {
			return nil
		}

		folder := filepath.Base(dir)

		date, _, ok := parseEMLFilename(name)
		if !ok {
			return nil
		}

		if dateStr != "" && date != dateStr {
			return nil
		}

		results = append(results, browseView.EmailView{
			Date:      date,
			Filename:  name,
			Folder:    folder,
			Size:      info.Size(),
			AccountID: acctID,
		})

		return nil
	}); err != nil {
		slog.Error("failed to scan email directory", "error", err)
	}

	return results
}

func (h *BrowseHandler) countEmailsForAccount(accountID int64) int64 {
	count := int64(0)

	baseDir := h.dataDir
	accountDir := filepath.Join(baseDir, strconv.FormatInt(accountID, 10))

	if _, err := os.Stat(accountDir); os.IsNotExist(err) {
		return 0
	}

	filepath.Walk(accountDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		name := info.Name()
		if strings.HasSuffix(name, ".eml") || strings.HasSuffix(name, ".eml.enc") {
			count++
		}
		return nil
	})

	return count
}

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

// ServeHTTP implements http.Handler for the browse handler.
func (h *BrowseHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	switch {
	case path == "/browse" && r.Method == http.MethodGet:
		h.List(w, r)
	case strings.HasPrefix(path, "/browse/") && r.Method == http.MethodGet:
		// Check if this is a download path: /browse/{account_id}/{date}/{filename}
		parts := strings.Split(strings.Trim(path, "/"), "/")
		if len(parts) >= 4 {
			h.Download(w, r)
		} else {
			http.NotFound(w, r)
		}
	default:
		http.NotFound(w, r)
	}
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

// Download handles browsing to a specific email file and downloading it.
// FIX #6: Validates path segments to prevent directory traversal.
// FIX #13: Decrypts .eml.enc files instead of serving raw ciphertext.
func (h *BrowseHandler) Download(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 4 {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	accountIDStr := parts[1]
	folder := parts[2]
	filename := parts[3]

	accountID, err := strconv.ParseInt(accountIDStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid account ID", http.StatusBadRequest)
		return
	}

	// FIX #6: Validate each path segment to prevent directory traversal.
	for _, seg := range []string{accountIDStr, folder, filename} {
		if seg == "" || seg == "." || seg == ".." ||
			strings.ContainsRune(seg, '/') || strings.ContainsRune(seg, '\\') ||
			strings.Contains(seg, "..") {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
	}

	encPath := filepath.Join(h.dataDir, accountIDStr, folder, filename)

	// Double-check the resolved path is still under dataDir.
	absDataDir, err := filepath.Abs(h.dataDir)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	absPath, err := filepath.Abs(encPath)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !strings.HasPrefix(absPath, absDataDir+string(filepath.Separator)) && absPath != absDataDir {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	if _, err := os.Stat(encPath); err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "file not found", http.StatusNotFound)
		} else {
			http.Error(w, "cannot access file", http.StatusInternalServerError)
		}
		return
	}

	// FIX #13: Decrypt .eml.enc files before serving.
	var plaintext []byte
	if h.keyWrapper != nil {
		plaintext, err = crypto.DecryptFile(encPath, h.keyWrapper)
		if err != nil {
			slog.Error("failed to decrypt file via browse", "path", encPath, "error", err)
			http.Error(w, "decryption failed", http.StatusInternalServerError)
			return
		}
	} else {
		plaintext, err = os.ReadFile(encPath)
		if err != nil {
			slog.Error("failed to read file via browse", "path", encPath, "error", err)
			http.Error(w, "cannot read file", http.StatusInternalServerError)
			return
		}
	}

	sanitized := sanitizeFilename(filename)
	w.Header().Set("Content-Type", "message/rfc822")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s.eml\"", sanitized))
	w.Header().Set("Content-Length", strconv.Itoa(len(plaintext)))
	w.WriteHeader(http.StatusOK)
	w.Write(plaintext)

	slog.Info("served email via browse", "account_id", accountID, "folder", folder, "filename", filename)
}
