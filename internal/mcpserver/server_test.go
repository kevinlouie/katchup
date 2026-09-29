package mcpserver_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pressly/goose/v3"

	"katchup/internal/account"
	"katchup/internal/api"
	"katchup/internal/crypto"
	"katchup/internal/imap"
	"katchup/internal/mcpserver"
	migrations "katchup/sql/migrations"

	_ "modernc.org/sqlite"
)

const testToken = "test-api-token"

// fixedNow is the clock newer_than_days is measured from.
var fixedNow = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

// testEML is a multipart/mixed message: a quoted-printable text/plain body, an
// HTML alternative, and a base64 PDF attachment.
const testEML = "From: =?UTF-8?B?7ZmN6ri464+Z?= <gildong@example.com>\r\n" +
	"To: kevin@example.com\r\n" +
	"Cc: Ops Team <ops@example.com>\r\n" +
	"Subject: Invoice for September\r\n" +
	"Date: Tue, 01 Sep 2026 09:30:00 +0900\r\n" +
	"Message-ID: <invoice-42@example.com>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=\"outer\"\r\n" +
	"\r\n" +
	"--outer\r\n" +
	"Content-Type: multipart/alternative; boundary=\"inner\"\r\n" +
	"\r\n" +
	"--inner\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"Content-Transfer-Encoding: quoted-printable\r\n" +
	"\r\n" +
	"Hi Kevin, the total is =E2=82=A9120,000.\r\n" +
	"--inner\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"\r\n" +
	"<p>Hi Kevin, the total is &#8361;120,000.</p>\r\n" +
	"--inner--\r\n" +
	"--outer\r\n" +
	"Content-Type: application/pdf; name=\"invoice.pdf\"\r\n" +
	"Content-Disposition: attachment; filename=\"invoice.pdf\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"JVBERi0xLjQK\r\n" +
	"--outer--\r\n"

type fixture struct {
	url       string
	accounts  *account.Store
	messages  *imap.Store
	accountID int64
	messageID int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()

	db, err := sql.Open("sqlite", filepath.Join(dataDir, "katchup.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.Up(db, "."); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	const masterKey = "test-master-key-for-mcp-tests"
	kw, err := crypto.NewMasterKeyWrapper(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := account.New(db, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := imap.NewStore(db, accounts)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := accounts.CreateAccount(ctx, "Personal", "imap.example.com", 993, "kevin@example.com", "enc", true, []string{"INBOX"})
	if err != nil {
		t.Fatal(err)
	}

	relPath := filepath.Join("1", "INBOX", "2026-09-01_7.eml.enc")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dataDir, relPath)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := crypto.EncryptFile(filepath.Join(dataDir, relPath), []byte(testEML), kw); err != nil {
		t.Fatal(err)
	}
	blobID, err := messages.UpsertBlob(ctx, acct.ID, "sha-invoice", relPath, int64(len(testEML)))
	if err != nil {
		t.Fatal(err)
	}
	if err := messages.InsertMessage(ctx, imap.InsertMessageParams{
		AccountID: acct.ID, Folder: "INBOX", UID: 7, BlobID: blobID,
		MessageIDHdr: "invoice-42@example.com", FuzzyFP: "fp-invoice",
		FromAddr: "gildong@example.com", ToAddr: "kevin@example.com",
		Subject: "Invoice for September", InternalDate: "2026-09-01T00:30:00Z", Size: int64(len(testEML)),
	}); err != nil {
		t.Fatal(err)
	}
	a, found, err := messages.LookupArchived(ctx, "invoice-42@example.com", "")
	if err != nil || !found {
		t.Fatalf("seeded message not found: %v", err)
	}

	h := mcpserver.Handler(mcpserver.Deps{
		Accounts: accounts, Messages: messages, Syncer: imap.NewSyncer(messages, dataDir, kw, accounts),
		DataDir: dataDir, KeyWrapper: kw, CoalesceWindow: time.Minute,
		Now: func() time.Time { return fixedNow },
	})
	mux := http.NewServeMux()
	mux.Handle("/api/mcp", h)
	srv := httptest.NewServer(api.APIAuth(testToken, mux))
	t.Cleanup(srv.Close)

	return &fixture{url: srv.URL + "/api/mcp", accounts: accounts, messages: messages, accountID: acct.ID, messageID: a.ID}
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func connect(t *testing.T, f *fixture) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   f.url,
		HTTPClient: &http.Client{Transport: bearer{testToken}},
		MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// call invokes a tool and decodes its structured result into out. It fails the
// test on a tool error unless wantErr is set.
func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if out != nil && !res.IsError {
		raw, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
	}
	return res
}

func TestListTools(t *testing.T) {
	cs := connect(t, newFixture(t))
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tool := range res.Tools {
		got[tool.Name] = tool.Annotations != nil && tool.Annotations.ReadOnlyHint
		if tool.Name == "search_messages" {
			// Only query is required; the date/paging inputs are optional.
			var schema struct {
				Required   []string       `json:"required"`
				Properties map[string]any `json:"properties"`
			}
			raw, _ := json.Marshal(tool.InputSchema)
			json.Unmarshal(raw, &schema)
			if len(schema.Required) != 1 || schema.Required[0] != "query" || schema.Properties["newer_than_days"] == nil {
				t.Errorf("search_messages schema: required=%v properties=%v", schema.Required, schema.Properties)
			}
		}
	}
	for _, name := range []string{"list_accounts", "search_messages", "list_messages", "get_message", "check_archived", "sync_status"} {
		if ro, ok := got[name]; !ok || !ro {
			t.Errorf("tool %s: present=%v readOnly=%v", name, ok, ro)
		}
	}
	if ro, ok := got["trigger_sync"]; !ok || ro {
		t.Errorf("trigger_sync: present=%v readOnly=%v, want present and not read-only", ok, ro)
	}
}

func TestGetMessage(t *testing.T) {
	f := newFixture(t)
	cs := connect(t, f)

	var m map[string]any
	call(t, cs, "get_message", map[string]any{"id": f.messageID}, &m)
	var out struct {
		Attachments []mcpserver.Attachment `json:"attachments"`
	}
	call(t, cs, "get_message", map[string]any{"id": f.messageID}, &out)

	if want := "홍길동 <gildong@example.com>"; m["from"] != want {
		t.Errorf("from = %q, want %q", m["from"], want)
	}
	if m["cc"] != "Ops Team <ops@example.com>" {
		t.Errorf("cc = %q", m["cc"])
	}
	if m["body_format"] != "text/plain" || m["body"] != "Hi Kevin, the total is ₩120,000." {
		t.Errorf("body = %q (%v)", m["body"], m["body_format"])
	}
	if m["date"] != "2026-09-01T09:30:00+09:00" {
		t.Errorf("date = %q", m["date"])
	}
	if len(out.Attachments) != 1 || out.Attachments[0].Filename != "invoice.pdf" || out.Attachments[0].Size != 9 {
		t.Errorf("attachments = %+v", out.Attachments)
	}

	call(t, cs, "get_message", map[string]any{"id": f.messageID, "max_chars": 8}, &m)
	if m["body"] != "Hi Kevin" || m["truncated"] != true || m["body_chars"] != float64(32) {
		t.Errorf("truncated: body=%q truncated=%v chars=%v", m["body"], m["truncated"], m["body_chars"])
	}

	if res := call(t, cs, "get_message", map[string]any{"id": 9999}, nil); !res.IsError {
		t.Error("missing message: want tool error")
	}
}

func TestSearchAndList(t *testing.T) {
	f := newFixture(t)
	cs := connect(t, f)

	var s struct {
		Backend string `json:"backend"`
		Results []struct {
			ID      int64  `json:"id"`
			Subject string `json:"subject"`
		} `json:"results"`
	}
	call(t, cs, "search_messages", map[string]any{"query": "invoice"}, &s)
	if s.Backend != "database" || len(s.Results) != 1 || s.Results[0].ID != f.messageID {
		t.Errorf("search = %+v", s)
	}

	var l struct {
		Total    int64 `json:"total"`
		Messages []struct {
			ID int64 `json:"id"`
		} `json:"messages"`
	}
	call(t, cs, "list_messages", map[string]any{"account_id": f.accountID, "since": "2026-09-01", "until": "2026-09-01"}, &l)
	if l.Total != 1 || len(l.Messages) != 1 {
		t.Errorf("list on day = %+v", l)
	}
	call(t, cs, "list_messages", map[string]any{"since": "2026-09-02"}, &l)
	if l.Total != 0 {
		t.Errorf("list after day total = %d", l.Total)
	}
	for _, args := range []map[string]any{
		{"since": "Sept 1"},
		{"since": "2026-09-02", "until": "2026-09-01"},
		{"since": "2026-09-01", "newer_than_days": 5},
		{"newer_than_days": -1},
	} {
		if res := call(t, cs, "list_messages", args, nil); !res.IsError {
			t.Errorf("%v: want tool error", args)
		}
	}
}

// seedHeader indexes a message row without a blob on disk; enough for the
// search and list tools, which never read bodies.
func seedHeader(t *testing.T, f *fixture, uid int64, subject, date string) {
	t.Helper()
	ctx := context.Background()
	blobID, err := f.messages.UpsertBlob(ctx, f.accountID, "sha-"+subject, "1/INBOX/x.eml.enc", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.messages.InsertMessage(ctx, imap.InsertMessageParams{
		AccountID: f.accountID, Folder: "INBOX", UID: uid, BlobID: blobID,
		FromAddr: "noreply@airline.example", Subject: subject, InternalDate: date,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDateRangeAndPaging(t *testing.T) {
	f := newFixture(t)
	// fixedNow is 2026-09-29T12:00Z; the fixture's invoice is 2026-09-01T00:30Z.
	seedHeader(t, f, 100, "Flight to Seoul", "2026-09-25T08:00:00Z")
	seedHeader(t, f, 101, "Flight to Oslo", "2026-09-10T08:00:00Z")
	seedHeader(t, f, 102, "Flight to Berlin", "2026-08-01T08:00:00Z")
	seedHeader(t, f, 103, "Flight to Paris", "2026-08-18T23:59:59Z")
	cs := connect(t, f)

	type page struct {
		Order   string `json:"order"`
		HasMore bool   `json:"has_more"`
		Results []struct {
			Subject string `json:"subject"`
		} `json:"results"`
	}
	subjects := func(p page) []string {
		var out []string
		for _, r := range p.Results {
			out = append(out, r.Subject)
		}
		return out
	}

	// 40 days before fixedNow is 2026-08-20T12:00Z: Seoul and Oslo only.
	var p page
	call(t, cs, "search_messages", map[string]any{"query": "flight", "newer_than_days": 40}, &p)
	if got := subjects(p); len(got) != 2 || got[0] != "Flight to Seoul" || got[1] != "Flight to Oslo" || p.HasMore || p.Order != "newest_first" {
		t.Errorf("newer_than_days=40: %v has_more=%v order=%s", got, p.HasMore, p.Order)
	}

	// until is inclusive of the whole day.
	call(t, cs, "search_messages", map[string]any{"query": "flight", "since": "2026-08-18", "until": "2026-08-18"}, &p)
	if got := subjects(p); len(got) != 1 || got[0] != "Flight to Paris" {
		t.Errorf("single day: %v", got)
	}

	// Paging walks all four flights newest first, two at a time.
	var all []string
	for n := 1; ; n++ {
		call(t, cs, "search_messages", map[string]any{"query": "flight", "limit": 2, "page": n}, &p)
		all = append(all, subjects(p)...)
		if !p.HasMore {
			break
		}
		if n > 3 {
			t.Fatal("has_more never went false")
		}
	}
	if want := []string{"Flight to Seoul", "Flight to Oslo", "Flight to Paris", "Flight to Berlin"}; strings.Join(all, "|") != strings.Join(want, "|") {
		t.Errorf("paged = %v, want %v", all, want)
	}

	var l struct {
		Total   int64 `json:"total"`
		HasMore bool  `json:"has_more"`
	}
	call(t, cs, "list_messages", map[string]any{"newer_than_days": 40, "per_page": 2}, &l)
	if l.Total != 3 || !l.HasMore {
		t.Errorf("list newer_than_days=40: %+v (want invoice+Seoul+Oslo, has_more)", l)
	}
	call(t, cs, "list_messages", map[string]any{"newer_than_days": 40, "per_page": 2, "page": 2}, &l)
	if l.HasMore {
		t.Errorf("list page 2: has_more = true")
	}
}

func TestCheckArchived(t *testing.T) {
	f := newFixture(t)
	cs := connect(t, f)

	var out struct {
		Results []struct {
			Archived bool  `json:"archived"`
			ID       int64 `json:"id"`
		} `json:"results"`
	}
	call(t, cs, "check_archived", map[string]any{"messages": []map[string]string{
		{"message_id": "<invoice-42@example.com>"},
		{"message_id": "nope@example.com", "fp": "fp-invoice"},
		{"message_id": "nope@example.com"},
	}}, &out)
	if len(out.Results) != 3 {
		t.Fatalf("results = %+v", out.Results)
	}
	if !out.Results[0].Archived || out.Results[0].ID != f.messageID {
		t.Errorf("bracketed message-id: %+v", out.Results[0])
	}
	if !out.Results[1].Archived {
		t.Errorf("fp fallback: %+v", out.Results[1])
	}
	if out.Results[2].Archived {
		t.Errorf("unknown: %+v", out.Results[2])
	}
}

func TestAccountsAndSyncStatus(t *testing.T) {
	f := newFixture(t)
	cs := connect(t, f)

	var accts struct {
		Accounts []struct {
			ID           int64  `json:"id"`
			Username     string `json:"username"`
			MessageCount int64  `json:"message_count"`
		} `json:"accounts"`
	}
	call(t, cs, "list_accounts", map[string]any{}, &accts)
	if len(accts.Accounts) != 1 || accts.Accounts[0].MessageCount != 1 || accts.Accounts[0].Username != "kevin@example.com" {
		t.Errorf("accounts = %+v", accts)
	}

	var st struct {
		Runs []account.SyncRun `json:"runs"`
	}
	call(t, cs, "sync_status", map[string]any{"account_id": f.accountID}, &st)
	if len(st.Runs) != 0 {
		t.Errorf("runs = %+v", st.Runs)
	}
	for _, tool := range []string{"sync_status", "trigger_sync"} {
		if res := call(t, cs, tool, map[string]any{"account_id": 999}, nil); !res.IsError {
			t.Errorf("%s unknown account: want tool error", tool)
		}
	}
}

// TestLegacyStatelessClient drives the endpoint the way a 2025-11-25
// streamable-HTTP client (open-webui's Python SDK) does: initialize, then a
// tools/call with no Mcp-Session-Id.
func TestLegacyStatelessClient(t *testing.T) {
	f := newFixture(t)

	post := func(body string, token string) (*http.Response, map[string]any) {
		req, _ := http.NewRequest(http.MethodPost, f.url, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2025-11-25")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var m map[string]any
		json.Unmarshal(bytes.TrimSpace(raw), &m)
		return resp, m
	}

	if resp, _ := post(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: status %d, want 401", resp.StatusCode)
	}

	resp, m := post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"open-webui","version":"0.11.3"}}}`, testToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize: status %d", resp.StatusCode)
	}
	if resp.Header.Get("Mcp-Session-Id") != "" {
		t.Error("stateless server set Mcp-Session-Id")
	}
	result, _ := m["result"].(map[string]any)
	if result["protocolVersion"] != "2025-11-25" {
		t.Errorf("negotiated %v, want 2025-11-25", result["protocolVersion"])
	}

	resp, m = post(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_messages","arguments":{"query":"invoice"}}}`, testToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools/call: status %d", resp.StatusCode)
	}
	result, _ = m["result"].(map[string]any)
	if result == nil || result["isError"] == true {
		t.Fatalf("tools/call result = %v", m)
	}
	content, _ := result["content"].([]any)
	if len(content) == 0 || !strings.Contains(content[0].(map[string]any)["text"].(string), "Invoice for September") {
		t.Errorf("text content missing search hit: %v", content)
	}
}

// TestReviewRegressions covers fixes from the adversarial review.
func TestReviewRegressions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// A row as IMAP sync stores it (bracketed Message-ID) and one with no known
	// date (backfill of a message with an unparseable Date header).
	for uid, row := range map[int64][2]string{
		200: {"<synced-1@example.com>", "2026-09-20T10:00:00Z"},
		201: {"undated@example.com", ""},
	} {
		blobID, err := f.messages.UpsertBlob(ctx, f.accountID, row[0], "1/INBOX/r.eml.enc", 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.messages.InsertMessage(ctx, imap.InsertMessageParams{
			AccountID: f.accountID, Folder: "INBOX", UID: uid, BlobID: blobID,
			MessageIDHdr: row[0], Subject: "regression", InternalDate: row[1],
		}); err != nil {
			t.Fatal(err)
		}
	}
	cs := connect(t, f)

	var arch struct {
		Results []struct {
			Archived bool `json:"archived"`
		} `json:"results"`
	}
	call(t, cs, "check_archived", map[string]any{"messages": []map[string]string{
		{"message_id": "<synced-1@example.com>"}, {"message_id": "synced-1@example.com"},
	}}, &arch)
	if len(arch.Results) != 2 || !arch.Results[0].Archived || !arch.Results[1].Archived {
		t.Errorf("bracketed synced row: %+v", arch.Results)
	}

	// An unknown date matches neither side of a bound, but is listed unbounded.
	var l struct {
		Total int64 `json:"total"`
	}
	call(t, cs, "list_messages", map[string]any{"until": "2026-12-31"}, &l)
	if l.Total != 2 { // invoice + synced-1, not the undated row
		t.Errorf("until-only total = %d, want 2", l.Total)
	}
	call(t, cs, "list_messages", map[string]any{}, &l)
	if l.Total != 3 {
		t.Errorf("unbounded total = %d, want 3", l.Total)
	}

	for _, args := range []map[string]any{
		{"query": "x", "account_id": -1},
		{"query": "x", "page": 1 << 40},
	} {
		if res := call(t, cs, "search_messages", args, nil); !res.IsError {
			t.Errorf("%v: want tool error", args)
		}
	}

	var m map[string]any
	call(t, cs, "get_message", map[string]any{"id": f.messageID}, &m)
	if m["internal_date"] != "2026-09-01T00:30:00Z" || m["date"] != "2026-09-01T09:30:00+09:00" {
		t.Errorf("dates: date=%v internal_date=%v", m["date"], m["internal_date"])
	}

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name != "check_archived" {
			continue
		}
		raw, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Properties map[string]struct {
				Type any `json:"type"`
			} `json:"properties"`
		}
		json.Unmarshal(raw, &schema)
		if schema.Properties["messages"].Type != "array" {
			t.Errorf("check_archived messages type = %v, want \"array\"", schema.Properties["messages"].Type)
		}
	}
}
