# IMAP Sync Specification

## Overview
The sync worker connects to each configured IMAP account, fetches new/unseen emails, and stores them as `.eml` files on disk. Runs on a 15-minute schedule via a ticker in `main.go` (systemd timer can also trigger it).

## Sync Process

### 1. Connect
- Dial IMAP server with TLS (port 993) or STARTTLS (port 143)
- Login with username + decrypted password
- Connection pool: one connection per account per sync run

### 2. Select Folders
- For each configured folder (default: `INBOX`):
  - `SELECT folder_name`
  - If folder doesn't exist, skip with log warning

### 3. Fetch New Messages
- Use `UID SEARCH UNSEEN` to find unseen messages
- For each UID returned:
  - `UID FETCH <uid> RFC822` to get full message
  - Check if `.eml` file already exists on disk (dedup)
  - If new, write `.eml` file
  - Mark as seen: `UID STORE <uid> +FLAGS (\Seen)`

### 4. Write .eml Files
- Path: `data/{account_id}/{folder}/{YYYY-MM-DD}_{uid}.eml`
- Use atomic write: write to temp file, then rename
- Permissions: `0600` (owner read/write only)

### 5. Track State
- Update `sync_runs.last_uid` with highest UID seen
- Update `sync_runs.emails_backed_up` with count
- On error, store error message and set `status = 'failed'`
- On success, set `status = 'completed'`

## Folder Mapping

### Default
Only `INBOX` is synced.

### Configurable
User specifies folders in the account's `folders` field (comma-separated):
- `INBOX`
- `Sent`
- `Archive`
- `Trash`
- Custom folders

Folder names are case-sensitive and server-dependent. Gmail uses `[Gmail]/Sent Mail`, `[Gmail]/Trash`, etc.

## Deduplication

### UID-Based
- IMAP UIDs are unique per folder per account
- Store `last_uid` per account in `sync_runs`
- On next sync, use `UID SEARCH SINCE <date> UID <last_uid>+` to avoid re-scanning
- If `last_uid` is 0 (first sync), fetch all unseen

### File-Based
- Check if `data/{account_id}/{folder}/{date}_{uid}.eml` exists before writing
- Skip if exists (defensive, in case of partial sync)

## Error Handling

### Connection Errors
- Retry with exponential backoff (3 attempts, 1s → 2s → 4s)
- If all retries fail, log error and set sync status to `failed`
- Next scheduled run will retry

### Per-Message Errors
- Log individual fetch failures (e.g., malformed message)
- Continue with remaining messages
- Store up to 5 error messages in `sync_runs.errors`

### Timeout
- IMAP connection timeout: 30 seconds
- Fetch timeout per message: 60 seconds

## Scheduled Execution

### Ticker (in main.go)
```go
ticker := time.NewTicker(15 * time.Minute)
defer ticker.Stop()

go func() {
    // Run immediately on startup
    syncAll()
    for range ticker.C {
        syncAll()
    }
}()
```

### Manual Trigger
- `POST /sync/{account_id}/trigger` starts a sync run in a goroutine
- Returns immediately with 303 redirect
- Running syncs are tracked in `sync_runs` with `status = 'running'`
- Concurrent syncs: one per account (skip if already running)

## File Layout

```
data/
├── katchup.db                    # SQLite database (gitignored)
└── 1/                            # account_id directory
    ├── INBOX/
    │   ├── 2026-07-01_12345.eml
    │   ├── 2026-07-01_12346.eml
    │   └── 2026-07-02_12350.eml
    ├── Sent/
    │   └── 2026-07-01_12300.eml
    └── .eml.enc                  # encrypted files (if encryption enabled)
        ├── 2026-07-01_12345.eml.enc
        └── 2026-07-01_12346.eml.enc
```

## Testing

### Mock IMAP Server
Use `github.com/emersion/go-imap`'s built-in server for testing, or write a simple TCP listener that responds to IMAP commands.

### Test Cases
- Connect + fetch unseen + write .eml + update state
- Dedup: second sync skips already-seen UIDs
- Error: malformed message logged, continues with others
- Error: connection timeout after retries, marks failed
- Multiple folders: syncs all configured folders
- STARTTLS vs IMAPS: both connection modes work
