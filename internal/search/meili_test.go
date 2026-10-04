package search

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeMeili records the last JSON body posted to each path and answers searches
// with no hits.
func fakeMeili(t *testing.T) (*Meili, map[string]json.RawMessage) {
	t.Helper()
	bodies := map[string]json.RawMessage{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies[r.URL.Path] = raw
		w.Write([]byte(`{"hits":[]}`))
	}))
	t.Cleanup(srv.Close)
	return NewMeili(srv.URL, "key"), bodies
}

func TestMeiliSearchFiltersAndOffset(t *testing.T) {
	m, bodies := fakeMeili(t)
	since := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	before := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)

	if _, err := m.Search(context.Background(), Query{
		Text: "flight", AccountID: 2, Since: since, Before: before, Limit: 21, Offset: 40,
	}); err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	json.Unmarshal(bodies["/indexes/messages/search"], &got)
	if want := "account_id = 2 AND date_ts >= 1787184000 AND date_ts < 1790726400"; got["filter"] != want {
		t.Errorf("filter = %v, want %q", got["filter"], want)
	}
	if got["offset"] != float64(40) || got["limit"] != float64(21) {
		t.Errorf("offset/limit = %v/%v", got["offset"], got["limit"])
	}

	m.Search(context.Background(), Query{Text: "x"})
	got = nil
	json.Unmarshal(bodies["/indexes/messages/search"], &got)
	if _, ok := got["filter"]; ok {
		t.Errorf("unbounded query sent filter %v", got["filter"])
	}
}

func TestMeiliIndexAddsDateTS(t *testing.T) {
	m, bodies := fakeMeili(t)
	if err := m.Index(context.Background(), Doc{ID: 1, Date: "2026-08-20T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	var docs []map[string]any
	json.Unmarshal(bodies["/indexes/messages/documents"], &docs)
	if len(docs) != 1 || docs[0]["date_ts"] != float64(1787184000) || docs[0]["date"] != "2026-08-20T00:00:00Z" {
		t.Errorf("indexed docs = %v", docs)
	}
}

// TestMeiliSearchSetsUpMissingIndex: a search that fails because the filter
// settings are missing (Meili was down at startup or lost its data) configures
// the index and retries.
func TestMeiliSearchSetsUpMissingIndex(t *testing.T) {
	configured := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/indexes/messages/settings":
			configured = true
			w.Write([]byte(`{}`))
		case r.URL.Path == "/indexes/messages/search" && !configured:
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"code":"invalid_search_filter"}`))
		default:
			w.Write([]byte(`{"hits":[]}`))
		}
	}))
	t.Cleanup(srv.Close)

	if _, err := NewMeili(srv.URL, "key").Search(context.Background(), Query{Text: "x", AccountID: 1}); err != nil {
		t.Fatalf("search after index setup: %v", err)
	}
	if !configured {
		t.Fatal("index settings were not applied")
	}
}
