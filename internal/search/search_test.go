package search

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestDocHasNoBodyField is the load-bearing S10 guarantee: the search document
// indexed on message store must carry HEADERS ONLY — never the message body or
// attachment text. Assert the serialized JSON contains only header keys.
func TestDocHasNoBodyField(t *testing.T) {
	doc := Doc{
		ID:        7,
		AccountID: 3,
		Folder:    "INBOX",
		MessageID: "<abc@example.com>",
		From:      "sender@example.com",
		To:        "rcpt@example.com",
		Subject:   "Quarterly report",
		Date:      "2026-07-01T12:00:00Z",
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal doc: %v", err)
	}

	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal doc: %v", err)
	}

	forbidden := []string{"body", "text", "html", "content", "attachment", "raw", "eml"}
	for key := range fields {
		for _, bad := range forbidden {
			if strings.Contains(strings.ToLower(key), bad) {
				t.Errorf("indexed doc must not contain %q-like field, found %q", bad, key)
			}
		}
	}

	want := map[string]bool{
		"id": true, "account_id": true, "folder": true, "message_id": true,
		"from": true, "to": true, "subject": true, "date": true,
	}
	if len(fields) != len(want) {
		t.Errorf("expected exactly %d header fields, got %d: %v", len(want), len(fields), fields)
	}
	for key := range fields {
		if !want[key] {
			t.Errorf("unexpected field in indexed doc: %q", key)
		}
	}
}

// TestNoopIndexerDoesNothing verifies the disabled-search default never errors.
func TestNoopIndexerDoesNothing(t *testing.T) {
	if err := (NoopIndexer{}).Index(context.Background(), Doc{ID: 1}); err != nil {
		t.Fatalf("noop indexer returned error: %v", err)
	}
}
