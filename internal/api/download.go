package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"katchup/internal/account"
	"katchup/internal/crypto"
	"katchup/internal/imap"
)

type DownloadHandler struct {
	store      *account.Store
	imapStore  *imap.Store
	syncer     *imap.Syncer
	dataDir    string
	keyWrapper crypto.KeyWrapper
}

func NewDownloadHandler(store *account.Store, imapStore *imap.Store, syncer *imap.Syncer, dataDir string, keyWrapper crypto.KeyWrapper) *DownloadHandler {
	return &DownloadHandler{
		store:      store,
		imapStore:  imapStore,
		syncer:     syncer,
		dataDir:    dataDir,
		keyWrapper: keyWrapper,
	}
}

func (h *DownloadHandler) Handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse the path: /download/{account_id}/{date}/{filename}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 4 {
		http.Error(w, "invalid path - expected /download/{account_id}/{date}/{filename}", http.StatusBadRequest)
		return
	}

	accountIDStr := parts[1]
	date := parts[2]
	filename := parts[3]

	accountID, err := strconv.ParseInt(accountIDStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid account ID", http.StatusBadRequest)
		return
	}

	// Validate every path segment to prevent path traversal.
	// Each segment must be a local filename (no separators, no ..).
	for _, seg := range []string{accountIDStr, date, filename} {
		if seg == "" || seg == "." || seg == ".." ||
			strings.ContainsRune(seg, '/') || strings.ContainsRune(seg, '\\') ||
			strings.Contains(seg, "..") {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
	}

	// Build the file path using filepath.Join (which handles the segments safely).
	encPath := filepath.Join(h.dataDir, accountIDStr, date, filename)

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

	// Verify the file exists
	if _, err := os.Stat(encPath); err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "file not found", http.StatusNotFound)
		} else {
			http.Error(w, "cannot access file", http.StatusInternalServerError)
		}
		return
	}

	// Decrypt the file
	var plaintext []byte
	if h.keyWrapper != nil {
		plaintext, err = crypto.DecryptFile(encPath, h.keyWrapper)
		if err != nil {
			slog.Error("failed to decrypt file", "path", encPath, "error", err)
			http.Error(w, "decryption failed", http.StatusInternalServerError)
			return
		}
	} else {
		plaintext, err = os.ReadFile(encPath)
		if err != nil {
			slog.Error("failed to read file", "path", encPath, "error", err)
			http.Error(w, "cannot read file", http.StatusInternalServerError)
			return
		}
	}

	// Serve the decrypted .eml file
	sanitized := sanitizeFilename(filename)
	w.Header().Set("Content-Type", "message/rfc822")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s.eml\"", sanitized))
	w.Header().Set("Content-Length", strconv.Itoa(len(plaintext)))
	w.WriteHeader(http.StatusOK)
	w.Write(plaintext)

	slog.Info("served decrypted email", "account_id", accountID, "date", date, "filename", filename)
}

func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "..", "")
	name = strings.ReplaceAll(name, "/", "")
	name = strings.ReplaceAll(name, "\\", "")
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}
