// Package mcpserver exposes katchup's archive to agents as an MCP server
// (streamable HTTP, stateless). It serves both the 2026-07-28 protocol and 2025
// streamable-HTTP clients such as open-webui, and is mounted under /api/mcp so it
// inherits the /api/* bearer-token auth.
//
// Unlike /search, get_message decrypts and returns message bodies: anything
// holding KATCHUP_API_TOKEN can read archived mail through it.
package mcpserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"katchup/internal/account"
	"katchup/internal/crypto"
	"katchup/internal/imap"
	"katchup/internal/search"
)

const (
	maxPage            = 100000
	defaultSearchLimit = 20
	maxSearchLimit     = 100
	defaultPerPage     = 50
	maxPerPage         = 100
	defaultMaxChars    = 20000
	maxMaxChars        = 200000
	maxArchivedBatch   = 500
	defaultRunLimit    = 5
)

// Deps are the katchup services the tools read from.
type Deps struct {
	Accounts *account.Store
	Messages *imap.Store
	Syncer   *imap.Syncer
	// Searcher is the Meilisearch backend, or nil to use the SQLite LIKE fallback.
	Searcher       search.Searcher
	DataDir        string
	KeyWrapper     crypto.KeyWrapper
	CoalesceWindow time.Duration
	// Now overrides the clock for newer_than_days (tests); nil means time.Now.
	Now func() time.Time
}

const instructions = `katchup is a read-only, encrypted backup of the user's IMAP mailboxes.
Message ids returned by search_messages / list_messages are katchup ids; pass them to get_message to read a message.
search_messages matches headers only (from/to/subject), never bodies.
Mail changed on the live server (read, moved, deleted) is not reflected here; call trigger_sync to pull new mail.`

// Handler returns the stateless streamable-HTTP handler serving the katchup tools.
func Handler(d Deps) http.Handler {
	srv := NewServer(d)
	return mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{
			Stateless:    true,
			JSONResponse: true,
			// The SDK's DNS-rebinding guard 403s any non-loopback Host when katchup
			// listens on loopback (e.g. behind a local reverse proxy). The bearer
			// token already defeats rebinding: a rebound browser page can't send it.
			DisableLocalhostProtection: true,
		},
	)
}

// NewServer builds the MCP server with every katchup tool registered.
func NewServer(d Deps) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "katchup", Version: "1"}, &mcp.ServerOptions{Instructions: instructions})
	t := &tools{d}

	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_accounts",
		Description: "List backed-up mailboxes with their folders, message counts, and latest sync run.",
		Annotations: readOnly,
	}, t.listAccounts)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "search_messages",
		Description: "Search archived mail by header text (from, to, subject); bodies are not searched. Ranked by relevance when Meilisearch serves it, else newest first. Page with page/limit until has_more is false." + dateRangeDesc,
		Annotations: readOnly,
	}, t.searchMessages)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_messages",
		Description: "Page through archived messages newest first, optionally filtered by account and date range. Use this to review everything in a window (e.g. newer_than_days) when keywords may miss." + dateRangeDesc,
		Annotations: readOnly,
	}, t.listMessages)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_message",
		Description: "Read one archived message: headers, decrypted text body (HTML converted to text), and attachment names/sizes. Attachment content is not returned.",
		Annotations: readOnly,
	}, t.getMessage)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "check_archived",
		Description: "Check whether messages are safely backed up, by Message-ID header, with an optional fuzzy-fingerprint fallback. Use before acting on live mail.",
		Annotations: readOnly,
		InputSchema: checkArchivedSchema(),
	}, t.checkArchived)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "sync_status",
		Description: "Show recent sync runs for an account (status, messages backed up, errors).",
		Annotations: readOnly,
	}, t.syncStatus)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "trigger_sync",
		Description: "Start a backup sync for an account without waiting for it. Coalesces with a running or just-finished run and respects provider throttle cooldowns; poll sync_status for progress.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: new(false), IdempotentHint: true},
	}, t.triggerSync)
	return s
}

type tools struct{ Deps }

// --- list_accounts ---

type listAccountsIn struct{}

type accountOut struct {
	ID           int64            `json:"id"`
	Name         string           `json:"name"`
	Username     string           `json:"username"`
	Folders      []string         `json:"folders"`
	IsSyncing    bool             `json:"is_syncing"`
	MessageCount int64            `json:"message_count"`
	LastSync     *account.SyncRun `json:"last_sync,omitempty"`
}

type listAccountsOut struct {
	Accounts []accountOut `json:"accounts"`
}

func (t *tools) listAccounts(ctx context.Context, _ *mcp.CallToolRequest, _ listAccountsIn) (*mcp.CallToolResult, listAccountsOut, error) {
	accts, err := t.Accounts.ListAccounts(ctx)
	if err != nil {
		return nil, listAccountsOut{}, err
	}
	out := listAccountsOut{Accounts: make([]accountOut, 0, len(accts))}
	for _, a := range accts {
		count, err := t.Messages.CountMessagesByAccount(ctx, a.ID)
		if err != nil {
			return nil, listAccountsOut{}, err
		}
		runs, err := t.Accounts.ListRecentRuns(ctx, a.ID)
		if err != nil {
			return nil, listAccountsOut{}, err
		}
		ao := accountOut{
			ID: a.ID, Name: a.Name, Username: a.Username, Folders: a.Folders,
			IsSyncing: a.IsSyncing, MessageCount: count,
		}
		if len(runs) > 0 {
			ao.LastSync = &runs[0]
		}
		out.Accounts = append(out.Accounts, ao)
	}
	return nil, out, nil
}

// --- search_messages / list_messages ---

type messageSummary struct {
	ID        int64  `json:"id"`
	AccountID int64  `json:"account_id"`
	Folder    string `json:"folder"`
	MessageID string `json:"message_id,omitempty"`
	From      string `json:"from"`
	To        string `json:"to,omitempty"`
	Subject   string `json:"subject"`
	Date      string `json:"date"`
}

// dateRangeDesc documents the shared since/until/newer_than_days inputs.
const dateRangeDesc = " Dates are UTC; since/until take YYYY-MM-DD (until is inclusive) or an RFC 3339 timestamp."

type searchIn struct {
	Query         string `json:"query" jsonschema:"text to match against from/to/subject headers"`
	AccountID     int64  `json:"account_id,omitempty" jsonschema:"restrict to one account id (from list_accounts); omit for all"`
	Since         string `json:"since,omitempty" jsonschema:"only messages on or after this date"`
	Until         string `json:"until,omitempty" jsonschema:"only messages on or before this date"`
	NewerThanDays int    `json:"newer_than_days,omitempty" jsonschema:"only messages from the last N days; use instead of since"`
	Page          int    `json:"page,omitempty" jsonschema:"1-based page, default 1"`
	Limit         int    `json:"limit,omitempty" jsonschema:"results per page, default 20, max 100"`
}

type searchOut struct {
	// Backend is "meilisearch" or "database" (SQLite LIKE over subject/from).
	Backend string `json:"backend"`
	// Order is "relevance" (meilisearch) or "newest_first" (database).
	Order   string           `json:"order"`
	Page    int              `json:"page"`
	HasMore bool             `json:"has_more"`
	Results []messageSummary `json:"results"`
}

func (t *tools) searchMessages(ctx context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, searchOut, error) {
	if in.Query == "" {
		return nil, searchOut{}, errors.New("query is required")
	}
	if err := checkPaging(in.AccountID, in.Page); err != nil {
		return nil, searchOut{}, err
	}
	since, before, err := t.dateRange(in.Since, in.Until, in.NewerThanDays)
	if err != nil {
		return nil, searchOut{}, err
	}
	page := max(in.Page, 1)
	limit := clamp(in.Limit, defaultSearchLimit, maxSearchLimit)
	// Fetch one extra row to learn whether another page exists.
	q := search.Query{
		Text: in.Query, AccountID: in.AccountID, Since: since, Before: before,
		Limit: limit + 1, Offset: (page - 1) * limit,
	}

	out := searchOut{Backend: "database", Order: "newest_first", Page: page}
	var results []search.Result
	if t.Searcher != nil {
		results, err = t.Searcher.Search(ctx, q)
		if err == nil {
			out.Backend, out.Order = "meilisearch", "relevance"
		} else {
			slog.Warn("mcp: meili search failed, falling back to database", "error", err)
		}
	}
	if out.Backend == "database" {
		results, err = t.Messages.SearchMessagesLike(ctx, q)
		if err != nil {
			return nil, searchOut{}, err
		}
	}

	if len(results) > limit {
		results, out.HasMore = results[:limit], true
	}
	out.Results = make([]messageSummary, len(results))
	for i, r := range results {
		out.Results[i] = messageSummary{
			ID: r.ID, AccountID: r.AccountID, Folder: r.Folder, MessageID: r.MessageID,
			From: r.From, Subject: r.Subject, Date: r.Date,
		}
	}
	return nil, out, nil
}

type listMessagesIn struct {
	AccountID     int64  `json:"account_id,omitempty" jsonschema:"restrict to one account id; omit for all"`
	Since         string `json:"since,omitempty" jsonschema:"only messages on or after this date"`
	Until         string `json:"until,omitempty" jsonschema:"only messages on or before this date"`
	NewerThanDays int    `json:"newer_than_days,omitempty" jsonschema:"only messages from the last N days; use instead of since"`
	Page          int    `json:"page,omitempty" jsonschema:"1-based page, default 1"`
	PerPage       int    `json:"per_page,omitempty" jsonschema:"default 50, max 100"`
}

type listMessagesOut struct {
	Total    int64            `json:"total"`
	Page     int              `json:"page"`
	PerPage  int              `json:"per_page"`
	HasMore  bool             `json:"has_more"`
	Messages []messageSummary `json:"messages"`
}

func (t *tools) listMessages(ctx context.Context, _ *mcp.CallToolRequest, in listMessagesIn) (*mcp.CallToolResult, listMessagesOut, error) {
	if err := checkPaging(in.AccountID, in.Page); err != nil {
		return nil, listMessagesOut{}, err
	}
	since, before, err := t.dateRange(in.Since, in.Until, in.NewerThanDays)
	if err != nil {
		return nil, listMessagesOut{}, err
	}
	page := max(in.Page, 1)
	perPage := clamp(in.PerPage, defaultPerPage, maxPerPage)
	filter := imap.MessageFilter{AccountID: in.AccountID, Since: since, Before: before}

	total, err := t.Messages.CountMessages(ctx, filter)
	if err != nil {
		return nil, listMessagesOut{}, err
	}
	offset := (page - 1) * perPage
	msgs, err := t.Messages.ListMessages(ctx, filter, int64(perPage), int64(offset))
	if err != nil {
		return nil, listMessagesOut{}, err
	}

	out := listMessagesOut{
		Total: total, Page: page, PerPage: perPage, HasMore: int64(offset+len(msgs)) < total,
		Messages: make([]messageSummary, len(msgs)),
	}
	for i, m := range msgs {
		out.Messages[i] = summarize(m)
	}
	return nil, out, nil
}

// dateRange turns the tool's since/until/newer_than_days inputs into a
// [since, before) window; a zero time leaves that side open.
// checkPaging validates the account/page inputs shared by search and list.
func checkPaging(accountID int64, page int) error {
	if accountID < 0 {
		return errors.New("account_id must be positive")
	}
	if page > maxPage {
		return fmt.Errorf("page must be at most %d", maxPage)
	}
	return nil
}

func (t *tools) dateRange(sinceStr, untilStr string, newerThanDays int) (since, before time.Time, err error) {
	if newerThanDays < 0 {
		return since, before, errors.New("newer_than_days must be positive")
	}
	if newerThanDays > 0 && sinceStr != "" {
		return since, before, errors.New("use either since or newer_than_days, not both")
	}
	if newerThanDays > 0 {
		since = t.now().UTC().AddDate(0, 0, -newerThanDays)
	}
	if sinceStr != "" {
		if since, _, err = parseDate(sinceStr); err != nil {
			return since, before, fmt.Errorf("since: %w", err)
		}
	}
	if untilStr != "" {
		until, dayOnly, err := parseDate(untilStr)
		if err != nil {
			return since, before, fmt.Errorf("until: %w", err)
		}
		// until is inclusive: the whole day for a date, the whole second
		// (internal_date's precision) for a timestamp.
		if dayOnly {
			before = until.AddDate(0, 0, 1)
		} else {
			before = until.Add(time.Second)
		}
	}
	if !since.IsZero() && !before.IsZero() && !since.Before(before) {
		return since, before, errors.New("since must be before until")
	}
	return since, before, nil
}

// parseDate accepts YYYY-MM-DD (midnight UTC) or RFC 3339; dayOnly reports which.
func parseDate(s string) (t time.Time, dayOnly bool, err error) {
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t, true, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, false, nil
	}
	return time.Time{}, false, fmt.Errorf("want YYYY-MM-DD or RFC 3339, got %q", s)
}

func (t *tools) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func summarize(m imap.Message) messageSummary {
	return messageSummary{
		ID: m.ID, AccountID: m.AccountID, Folder: m.Folder, MessageID: m.MessageIDHdr,
		From: m.FromAddr, To: m.ToAddr, Subject: m.Subject, Date: m.InternalDate,
	}
}

// --- get_message ---

type getMessageIn struct {
	ID       int64 `json:"id" jsonschema:"katchup message id (from search_messages or list_messages)"`
	MaxChars int   `json:"max_chars,omitempty" jsonschema:"truncate the body to this many characters, default 20000, max 200000"`
}

type getMessageOut struct {
	ID        int64  `json:"id"`
	AccountID int64  `json:"account_id"`
	Folder    string `json:"folder"`
	MessageID string `json:"message_id,omitempty"`
	From      string `json:"from"`
	To        string `json:"to,omitempty"`
	Cc        string `json:"cc,omitempty"`
	Subject   string `json:"subject"`
	// Date is the sender's Date header; InternalDate is when the server received
	// it (UTC), the value search/list results and date filters use.
	Date         string       `json:"date"`
	InternalDate string       `json:"internal_date"`
	BodyFormat   string       `json:"body_format,omitempty"`
	Body         string       `json:"body"`
	BodyChars    int          `json:"body_chars"`
	Truncated    bool         `json:"truncated"`
	Attachments  []Attachment `json:"attachments,omitempty"`
}

func (t *tools) getMessage(ctx context.Context, _ *mcp.CallToolRequest, in getMessageIn) (*mcp.CallToolResult, getMessageOut, error) {
	msg, err := t.Messages.GetMessageWithBlob(ctx, in.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, getMessageOut{}, fmt.Errorf("message %d not found", in.ID)
	}
	if err != nil {
		return nil, getMessageOut{}, err
	}

	raw, err := imap.ReadMessageBlob(t.DataDir, msg.BlobPath, t.KeyWrapper)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, getMessageOut{}, fmt.Errorf("message %d is indexed but its file is missing on disk", in.ID)
	}
	if err != nil {
		slog.Error("mcp: failed to read message blob", "message_id", msg.ID, "error", err)
		return nil, getMessageOut{}, fmt.Errorf("message %d could not be read", in.ID)
	}
	p, err := parseMail(raw)
	if err != nil {
		return nil, getMessageOut{}, fmt.Errorf("message %d: %w", in.ID, err)
	}
	slog.Info("mcp: served message", "message_id", msg.ID, "account_id", msg.AccountID, "folder", msg.Folder)

	body, truncated := truncateRunes(p.Body, clamp(in.MaxChars, defaultMaxChars, maxMaxChars))
	out := getMessageOut{
		ID: msg.ID, AccountID: msg.AccountID, Folder: msg.Folder,
		MessageID:    firstNonEmpty(p.MessageID, msg.MessageIDHdr),
		From:         firstNonEmpty(p.From, msg.FromAddr),
		To:           firstNonEmpty(p.To, msg.ToAddr),
		Cc:           p.Cc,
		Subject:      firstNonEmpty(p.Subject, msg.Subject),
		Date:         firstNonEmpty(p.Date, msg.InternalDate),
		InternalDate: msg.InternalDate,
		BodyFormat:   p.BodyFormat, Body: body, BodyChars: utf8.RuneCountInString(p.Body), Truncated: truncated,
		Attachments: p.Attachments,
	}
	return nil, out, nil
}

// --- check_archived ---

type archivedQuery struct {
	MessageID string `json:"message_id,omitempty" jsonschema:"RFC 5322 Message-ID header, with or without angle brackets"`
	FP        string `json:"fp,omitempty" jsonschema:"fuzzy fingerprint, tried when message_id misses"`
}

type checkArchivedIn struct {
	Messages []archivedQuery `json:"messages" jsonschema:"messages to check, max 500"`
}

type archivedResult struct {
	MessageID  string `json:"message_id,omitempty"`
	FP         string `json:"fp,omitempty"`
	Archived   bool   `json:"archived"`
	ArchivedAt string `json:"archived_at,omitempty"`
	ID         int64  `json:"id,omitempty"`
	Sha256     string `json:"sha256,omitempty"`
}

// checkArchivedSchema is the inferred input schema with "messages" narrowed from
// ["null","array"] (how nil slices infer) to "array": some function-calling
// backends reject union types.
func checkArchivedSchema() *jsonschema.Schema {
	schema, err := jsonschema.For[checkArchivedIn](nil)
	if err != nil {
		panic(err)
	}
	messages := schema.Properties["messages"]
	messages.Type, messages.Types = "array", nil
	return schema
}

type checkArchivedOut struct {
	Results []archivedResult `json:"results"`
}

func (t *tools) checkArchived(ctx context.Context, _ *mcp.CallToolRequest, in checkArchivedIn) (*mcp.CallToolResult, checkArchivedOut, error) {
	if len(in.Messages) == 0 {
		return nil, checkArchivedOut{}, errors.New("messages is required")
	}
	if len(in.Messages) > maxArchivedBatch {
		return nil, checkArchivedOut{}, fmt.Errorf("at most %d messages per call", maxArchivedBatch)
	}
	out := checkArchivedOut{Results: make([]archivedResult, len(in.Messages))}
	for i, q := range in.Messages {
		a, found, err := t.Messages.LookupArchived(ctx, q.MessageID, q.FP)
		if err != nil {
			return nil, checkArchivedOut{}, err
		}
		r := archivedResult{MessageID: q.MessageID, FP: q.FP, Archived: found}
		if found {
			r.ArchivedAt, r.ID, r.Sha256 = a.ArchivedAt, a.ID, a.Sha256
		}
		out.Results[i] = r
	}
	return nil, out, nil
}

// --- sync_status / trigger_sync ---

type syncStatusIn struct {
	AccountID int64 `json:"account_id" jsonschema:"account id (from list_accounts)"`
	Limit     int   `json:"limit,omitempty" jsonschema:"number of recent runs, default 5, max 20"`
}

type syncStatusOut struct {
	AccountID int64             `json:"account_id"`
	Runs      []account.SyncRun `json:"runs"`
}

func (t *tools) syncStatus(ctx context.Context, _ *mcp.CallToolRequest, in syncStatusIn) (*mcp.CallToolResult, syncStatusOut, error) {
	if err := t.requireAccount(ctx, in.AccountID); err != nil {
		return nil, syncStatusOut{}, err
	}
	runs, err := t.Accounts.ListRecentRuns(ctx, in.AccountID)
	if err != nil {
		return nil, syncStatusOut{}, err
	}
	// ListRecentRuns already caps at 20.
	if n := clamp(in.Limit, defaultRunLimit, len(runs)); n < len(runs) {
		runs = runs[:n]
	}
	return nil, syncStatusOut{AccountID: in.AccountID, Runs: runs}, nil
}

type triggerSyncIn struct {
	AccountID int64 `json:"account_id" jsonschema:"account id (from list_accounts)"`
}

type triggerSyncOut struct {
	RunID     int64 `json:"run_id"`
	AccountID int64 `json:"account_id"`
	// Started is false when the request joined a running or recently finished
	// run, or was skipped during a provider throttle cooldown.
	Started bool `json:"started"`
}

func (t *tools) triggerSync(ctx context.Context, _ *mcp.CallToolRequest, in triggerSyncIn) (*mcp.CallToolResult, triggerSyncOut, error) {
	if err := t.requireAccount(ctx, in.AccountID); err != nil {
		return nil, triggerSyncOut{}, err
	}
	runID, started, err := t.Syncer.TriggerSync(ctx, in.AccountID, t.CoalesceWindow)
	if err != nil {
		return nil, triggerSyncOut{}, err
	}
	return nil, triggerSyncOut{RunID: runID, AccountID: in.AccountID, Started: started}, nil
}

func (t *tools) requireAccount(ctx context.Context, id int64) error {
	if id <= 0 {
		return errors.New("account_id is required")
	}
	if _, err := t.Accounts.GetAccount(ctx, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("account %d not found", id)
		}
		return err
	}
	return nil
}

// --- helpers ---

// clamp returns def when v is unset (<= 0), otherwise v capped at hi.
func clamp(v, def, hi int) int {
	if v <= 0 {
		v = def
	}
	return min(v, hi)
}

// truncateRunes cuts s to at most n runes without copying it.
func truncateRunes(s string, n int) (string, bool) {
	count := 0
	for i := range s {
		if count == n {
			return s[:i], true
		}
		count++
	}
	return s, false
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
