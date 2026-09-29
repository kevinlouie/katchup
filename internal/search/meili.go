package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// indexName is the single Meili index holding header-only message docs.
const indexName = "messages"

// Meili is a minimal Meilisearch client built on net/http (no heavy SDK dep). It
// implements both Indexer and Searcher. It indexes ONLY header metadata — the Doc
// type has no body field, so a body can never be sent.
type Meili struct {
	baseURL string
	key     string
	client  *http.Client
}

// NewMeili builds a Meili client. url is the base endpoint (e.g.
// http://meilisearch:7700); key is the master/API key. No network call is made
// here — call EnsureIndex once at startup to create the index + filter settings.
func NewMeili(url, key string) *Meili {
	return &Meili{
		baseURL: strings.TrimRight(url, "/"),
		key:     key,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

// EnsureIndex creates the index (primary key "id") and marks account_id/folder/
// date_ts as filterable so account- and date-scoped search works. Best-effort: callers log and continue
// on error, since search degrades to the DB LIKE fallback if Meili is unreachable.
func (m *Meili) EnsureIndex(ctx context.Context) error {
	// Create the index (idempotent: an already-existing index returns an error we
	// ignore by not failing hard — the settings PATCH below still applies).
	createBody := map[string]string{"uid": indexName, "primaryKey": "id"}
	if _, err := m.do(ctx, http.MethodPost, "/indexes", createBody); err != nil {
		// index_already_exists is fine; only a transport error is worth reporting.
		if !strings.Contains(err.Error(), "index_already_exists") {
			return fmt.Errorf("create index: %w", err)
		}
	}
	settings := map[string]any{
		"filterableAttributes": []string{"account_id", "folder", "date_ts"},
		"searchableAttributes": []string{"subject", "from", "to", "message_id"},
	}
	if _, err := m.do(ctx, http.MethodPatch, "/indexes/"+indexName+"/settings", settings); err != nil {
		return fmt.Errorf("configure index settings: %w", err)
	}
	return nil
}

// meiliDoc is the stored document: Doc plus a numeric date_ts, because Meili
// only range-filters numbers. Docs indexed before date_ts existed lack it and
// drop out of date-bounded searches until `katchup reindex`.
type meiliDoc struct {
	Doc
	DateTS int64 `json:"date_ts,omitempty"`
}

// Index upserts a single header-only doc. Meili treats a POST of documents as an
// upsert keyed on the primary key ("id"), so re-indexing the same message is safe.
func (m *Meili) Index(ctx context.Context, doc Doc) error {
	md := meiliDoc{Doc: doc}
	if t, err := time.Parse(time.RFC3339, doc.Date); err == nil {
		md.DateTS = t.Unix()
	}
	if _, err := m.do(ctx, http.MethodPost, "/indexes/"+indexName+"/documents", []meiliDoc{md}); err != nil {
		return fmt.Errorf("index doc %d: %w", doc.ID, err)
	}
	return nil
}

// meiliSearchResponse is the subset of Meili's /search reply we consume.
type meiliSearchResponse struct {
	Hits []Doc `json:"hits"`
}

// Search runs a header-only query, ranked by relevance. Account and date bounds
// become Meili filters. Returns hits mapped to Result. Errors bubble up so the
// handler can fall back to the DB LIKE. Meili stops paging at its maxTotalHits
// (default 1000).
func (m *Meili) Search(ctx context.Context, q Query) ([]Result, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	body := map[string]any{"q": q.Text, "limit": limit, "offset": q.Offset}
	var filters []string
	if q.AccountID > 0 {
		filters = append(filters, "account_id = "+strconv.FormatInt(q.AccountID, 10))
	}
	if !q.Since.IsZero() {
		filters = append(filters, "date_ts >= "+strconv.FormatInt(q.Since.Unix(), 10))
	}
	if !q.Before.IsZero() {
		filters = append(filters, "date_ts < "+strconv.FormatInt(q.Before.Unix(), 10))
	}
	if len(filters) > 0 {
		body["filter"] = strings.Join(filters, " AND ")
	}
	raw, err := m.do(ctx, http.MethodPost, "/indexes/"+indexName+"/search", body)
	if err != nil {
		return nil, fmt.Errorf("meili search: %w", err)
	}
	var resp meiliSearchResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode search response: %w", err)
	}
	out := make([]Result, 0, len(resp.Hits))
	for _, h := range resp.Hits {
		out = append(out, Result{
			ID:        h.ID,
			AccountID: h.AccountID,
			Folder:    h.Folder,
			MessageID: h.MessageID,
			From:      h.From,
			Subject:   h.Subject,
			Date:      h.Date,
		})
	}
	return out, nil
}

// do issues a JSON request to the Meili API and returns the raw response body.
// A non-2xx status is turned into an error carrying the response payload (so the
// caller can detect codes like "index_already_exists").
func (m *Meili) do(ctx context.Context, method, path string, payload any) ([]byte, error) {
	var reader io.Reader
	if payload != nil {
		buf, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal payload: %w", err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if m.key != "" {
		req.Header.Set("Authorization", "Bearer "+m.key)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("meili %s %s: status %d: %s", method, path, resp.StatusCode, string(body))
	}
	return body, nil
}
