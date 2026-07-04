# IMAP Sync Specification

## Reality note (v1 shipped, corrections from the original draft)
- Sync is **non-mutating**: fetch uses `BODY.PEEK[]`, so it NEVER sets `\Seen`
  and never issues STORE/APPEND/EXPUNGE. The original "mark as seen" step is
  gone by design (katchup is a silent backup).
- Fetch is **batched**: `fetchBatchSize = 200` UIDs/batch, `fetchTimeout = 120s`
  per batch. Each batch persists its watermark (`folder_sync_state.last_uid`)
  and progress (`MarkSyncRunProgress`) before the next. This replaced a single
  bulk `UID FETCH` of all UIDs, which OOM'd/hung on large mailboxes.
- An aborted/timed-out batch returns the last **safe watermark** (not the max
  attempted UID) so unfetched UIDs retry next run.
- `MarkAllStaleRuns` fails every `running` sync at startup (a sync can't survive
  a process restart).

## Overview
The sync worker connects to each configured IMAP account, fetches new messages,
and stores them as encrypted `.eml.enc` files, indexed in the DB.

## Sync process
1. **Connect** — IMAPS (993) or STARTTLS (143), login with decrypted password.
2. **Select folders** — per configured folder; skip-with-warning if missing.
   For Gmail, prefer **All Mail** (retains everything regardless of labels — see
   Hermes note below).
3. **Discover UIDs** — `UID SEARCH` above `folder_sync_state.last_uid`.
4. **Batched fetch** — in chunks of `fetchBatchSize`, `UID FETCH <range>
   BODY.PEEK[]` (peek = no `\Seen`). Drain + persist per batch.
5. **Store** — see "Message index + dedup" below.
6. **Track state** — per batch: update `folder_sync_state.last_uid`,
   `sync_runs.emails_backed_up` (`MarkSyncRunProgress`). At end: `completed` or
   `failed` via `UpdateSyncRunStatus`.

## Message index + dedup (v2 / S7)
For each fetched message (raw RFC822 bytes `raw`):
1. `h = SHA-256(raw)` (hex).
2. Look up `blobs (account_id, sha256=h)`. If present → reuse (refcount++), do
   NOT rewrite the file. Else encrypt+write `.eml.enc` and insert a blob.
3. Insert a `messages` row: `(account_id, folder, uid, blob_id, message_id_hdr,
   fuzzy_fp, from, to, subject, internal_date, size)`.
   - `message_id_hdr` = RFC5322 `Message-ID` header.
   - `fuzzy_fp` = `sha256(lower(from)|internal_date|lower(subject))`.

## Scheduling & triggers (v2 / S9)
- **Scheduled floor**: a ticker runs every `KATCHUP_SYNC_INTERVAL` (default
  `6h`). This is the safety net — it must stay on even when Hermes drives.
- **On-demand**: `POST /api/sync?account=<id>` → async 202 with run id.
- **Per-account mutex**: at most one running sync per account. A trigger during
  a running sync joins it; a trigger within the coalesce window (default 30s of
  a completed run) returns that run. No stampede.

## Hermes note
Hermes triages the live mailbox and only touches mail katchup has archived
(it polls `/api/archived`). Because Hermes may label/move live mail, sync
Gmail's **All Mail** so a move never hides an unarchived message.

## File layout
```
data/
├── katchup.db
└── <account_id>/<folder>/<YYYY-MM-DD>_<uid>.eml.enc
```
Atomic write (temp + rename), perms 0600.

## Error handling
- Connection: retry with backoff (3 attempts). All fail → run `failed`.
- Per-message: log + continue; store up to 5 errors (bounded) in
  `sync_runs.errors` via the `appendErr` helper.
- Batch timeout: abort batch, keep safe watermark, retry next run.

## Testing
- Dedup: second identical fetch → refcount 2, one file, one blob.
- Non-mutating: fetch does not set `\Seen` (assert BODY.PEEK path).
- Batched watermark: aborted batch resumes from safe watermark.
- Trigger: concurrent triggers → one run.
