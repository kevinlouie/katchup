package api

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"katchup/internal/imap"
)

// ArchivedHandler serves the machine-facing archived-lookup API. It answers
// "has katchup already backed up this message?" so an external agent only acts
// on mail that is safely archived. Pull model: the caller holds the queue and retries.
type ArchivedHandler struct {
	store *imap.Store
}

func NewArchivedHandler(store *imap.Store) *ArchivedHandler {
	return &ArchivedHandler{store: store}
}

// ArchivedStatus is the per-message result. When archived is false the other
// fields are omitted.
type ArchivedStatus struct {
	Archived   bool   `json:"archived"`
	ArchivedAt string `json:"archived_at,omitempty"`
	ID         int64  `json:"id,omitempty"`
	Sha256     string `json:"sha256,omitempty"`
}

// batchLookupRequest is the POST /api/archived/lookup body.
type batchLookupRequest struct {
	MessageIDs []string `json:"message_ids"`
}

// Get handles GET /api/archived?message_id=<id>[&fp=<fuzzy_fp>]. It matches on
// the Message-ID header first; if that misses and a fp= param is supplied, it
// falls back to the fuzzy fingerprint.
func (h *ArchivedHandler) Get(w http.ResponseWriter, r *http.Request) {
	messageID := strings.TrimSpace(r.URL.Query().Get("message_id"))
	fp := strings.TrimSpace(r.URL.Query().Get("fp"))

	if messageID == "" && fp == "" {
		writeJSONError(w, http.StatusBadRequest, "message_id or fp is required")
		return
	}

	status, err := h.lookup(r, messageID, fp)
	if err != nil {
		slog.Error("archived lookup failed", "message_id", messageID, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	writeJSON(w, http.StatusOK, status)
}

// Lookup handles POST /api/archived/lookup with {message_ids:[...]}, returning a
// map of message-id -> status so a caller can check many at once.
func (h *ArchivedHandler) Lookup(w http.ResponseWriter, r *http.Request) {
	var req batchLookupRequest
	// Lenient decode: caller payloads may carry extra fields we don't model —
	// ignore them rather than 400 the whole batch.
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	out := make(map[string]ArchivedStatus, len(req.MessageIDs))
	for _, id := range req.MessageIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := out[id]; ok {
			continue
		}
		status, err := h.lookup(r, id, "")
		if err != nil {
			slog.Error("archived batch lookup failed", "message_id", id, "error", err)
			writeJSONError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		out[id] = status
	}

	writeJSON(w, http.StatusOK, out)
}

// lookup resolves a single message, trying the Message-ID first and falling back
// to the fuzzy fingerprint when a fp is supplied and the Message-ID missed.
func (h *ArchivedHandler) lookup(r *http.Request, messageID, fp string) (ArchivedStatus, error) {
	a, found, err := h.store.LookupArchived(r.Context(), messageID, fp)
	if err != nil || !found {
		return ArchivedStatus{Archived: false}, err
	}
	return ArchivedStatus{Archived: true, ArchivedAt: a.ArchivedAt, ID: a.ID, Sha256: a.Sha256}, nil
}

// ServeHTTP dispatches the archived-lookup routes.
func (h *ArchivedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/archived" && r.Method == http.MethodGet:
		h.Get(w, r)
	case r.URL.Path == "/api/archived/lookup" && r.Method == http.MethodPost:
		h.Lookup(w, r)
	default:
		writeJSONError(w, http.StatusNotFound, "not found")
	}
}

// APIAuth wraps the machine-facing /api/* routes with token auth. It fails
// closed: if the token is unset, EVERY /api/* request returns 503 (never
// silently open). The token must be presented as an "Authorization:
// Bearer <token>" header — query-param tokens are not accepted because URLs
// end up in access logs, proxies, and browser history.
func APIAuth(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" {
			writeJSONError(w, http.StatusServiceUnavailable, "api disabled: KATCHUP_API_TOKEN not set")
			return
		}
		if !validToken(token, extractToken(r)) {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// extractToken pulls the presented token from the Authorization bearer header.
func extractToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if rest, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// validToken compares in constant time; an empty presented token never matches.
func validToken(want, got string) bool {
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("failed to encode JSON response", "error", err)
	}
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
