# Ralph Fix Plan — Katchup

## IMPORTANT CONTEXT
v1 (Sprints S1–S6) is COMPLETE and deployed to a Synology NAS as
`ghcr.io/kevinlouie/katchup`. This plan now tracks **v2**: a proper message
index, content-hash dedup, a Hermes-facing API, header-only full-text search,
and a Gmail-API ingest fallback.

Sprint order for v2: **S7 → S8 → S9 → S10 → S11**. S8 depends on S7's schema.
Hermes integration needs S8 + S9. S10 (search) and S11 (Gmail API) are
independent and can trail.

Reality corrections (the original specs described features that were never
built — do not "restore" them):
- **Encryption is master-key AES-256-GCM only** (`FormatMasterKey = 0x02`).
  YubiKey PIV / RSA-2048 (`FormatRSA = 0x01`) was designed but NEVER
  implemented — no PIV library, no `ykman` runtime path. Treat as dead.
- **Sync is non-mutating**: fetch uses `BODY.PEEK[]`, never sets `\Seen`,
  never STORE/APPEND/EXPUNGE.
- **Dashboard lives at `/`** (not `/sync`).
- **Fetch is batched** (200 UIDs/batch, 120s/batch timeout) with per-batch
  watermark + progress persistence.

---

## Post-v1 Hardening — ALREADY DONE (do NOT redo)
- [x] Batched IMAP fetch (`fetchBatchSize=200`, `fetchTimeout=120s`) — fixed
      the "stuck at 0 / OOM on 39k-message bulk fetch" bug.
- [x] `BODY.PEEK[]` fetch — backup never marks live mail read.
- [x] `MarkSyncRunProgress` — incremental progress without closing the run.
- [x] `MarkAllStaleRuns` — fails every `running` run at startup (a sync can't
      survive a restart).
- [x] Dashboard moved to `GET /{$}`; UI redesigned (dark theme, ketchup accent).
- [x] Docker image built + pushed to ghcr (amd64), deployed via docker compose.

---

## Sprint S7: Message Index + Content-Hash Dedup [DONE]
**Goal**: Introduce a real message index and deduplicate identical mail by
content hash. Foundation for S8/S9/S10.

**Migration 003 (`003_message_index.sql`):**
```sql
CREATE TABLE blobs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id  INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    sha256      TEXT NOT NULL,             -- hex of SHA-256 over raw RFC822 bytes
    path        TEXT NOT NULL,             -- relative path to the .eml.enc file
    size        INTEGER NOT NULL,
    refcount    INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(account_id, sha256)             -- dedup key is PER-ACCOUNT
);
CREATE TABLE messages (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id     INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    folder         TEXT NOT NULL,
    uid            INTEGER NOT NULL,
    blob_id        INTEGER NOT NULL REFERENCES blobs(id) ON DELETE CASCADE,
    message_id_hdr TEXT,                   -- RFC5322 Message-ID header (indexed)
    fuzzy_fp       TEXT,                   -- sha256(normalized from|date|subject)
    from_addr      TEXT,
    to_addr        TEXT,
    subject        TEXT,
    internal_date  TEXT,
    size           INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(account_id, folder, uid)
);
CREATE INDEX idx_messages_msgid  ON messages(account_id, message_id_hdr);
CREATE INDEX idx_messages_fuzzy  ON messages(account_id, fuzzy_fp);
CREATE INDEX idx_messages_date   ON messages(account_id, internal_date DESC);
```

- [x] S7.1 Migration 003 (above).
- [x] S7.2 sqlc queries: `UpsertBlob` (insert or bump refcount, return id),
      `InsertMessage`, `GetBlobBySha`, `ListMessages` (paginated, replaces the
      filesystem walk), `CountMessages`.
- [x] S7.3 Sync path: after fetching raw bytes, compute `sha256(raw)`. If a blob
      for `(account_id, sha256)` exists → reuse it (bump refcount), do NOT
      rewrite the file. Else write `.eml.enc` and insert blob.
- [x] S7.4 Parse + store per message: `Message-ID`, `from`, `to`, `subject`,
      `internal_date`, and `fuzzy_fp = sha256(lower(from)|internal_date|lower(subject))`.
- [x] S7.5 Rewrite `browse` to read from `messages` (join `blobs`) instead of
      walking the filesystem. Download resolves `blob.path`.
- [x] S7.6 Backfill decision: **NUKE existing test data + re-sync** (current
      inbox is a disposable test). Document in AGENT.md; no migration script for
      old on-disk files.
- [x] S7.7 `go build ./...` + `go test ./...` clean.

**Tests:** dedup skips second identical fetch (refcount=2, one file); message row
per folder/uid; browse lists from DB; fuzzy_fp deterministic.

---

## Sprint S8: Archived-Lookup API (Hermes read side) [DONE]
**Goal**: Let Hermes ask "is this message archived?" so it only triages mail
katchup already backed up. **Pull model** (Hermes holds + retries).

- [x] S8.1 `GET /api/archived?message_id=<id>` →
      `{archived: bool, archived_at, id, sha256}`. Match on `message_id_hdr`;
      fall back to `fuzzy_fp` if a `fp=` param is given.
- [x] S8.2 `POST /api/archived/lookup` with `{message_ids: [...]}` → map of
      id→status (batch; Hermes checks many at once).
- [x] S8.3 Auth: accept the token via EITHER `Authorization: Bearer
      <KATCHUP_API_TOKEN>` OR `?token=<KATCHUP_API_TOKEN>` query param (Hermes
      header support unconfirmed — support both). If the env var is unset,
      `/api/*` returns 503 (fail closed) — never silently open.
- [x] S8.4 Docs: instruct syncing Gmail's **All Mail** so Hermes label/moves
      never hide an unarchived message from katchup. (See AGENT.md S8.4 note.)
- [x] S8.5 Tests: archived hit/miss by message-id; fuzzy fallback; 401 without
      token; 503 when token unset; batch lookup.

---

## Sprint S9: On-Demand Sync Trigger + Per-Account Mutex [DONE]
**Goal**: Hermes pokes katchup to sync a mailbox before polling. Safe against
stampede.

- [x] S9.1 Per-account **sync mutex/guard**: at most one running sync per
      account. Reuse `sync_runs` running-detection + an in-process lock. Run
      creation ALWAYS happens under the per-account exec lock (`Syncer.lockFor`);
      `beginRun` clears stale runs + rechecks the DB, so scheduled/trigger paths
      can never both create a run.
- [x] S9.2 `POST /api/sync?account=<id>` → **async 202** with the run id.
      `Syncer.TriggerSync(ctx, accountID, coalesceWindow)`: TryLock the exec lock;
      on success either coalesce onto a run finished < window ago or create a run
      + launch a background goroutine that holds the lock and releases it when the
      sync ends. On TryLock failure a sync is in flight → join it (return the
      running run id). Handler in internal/api/sync_trigger.go returns
      `{run_id, account_id, started}`.
- [x] S9.3 Kept scheduled sync as a **safety-net floor** (main.go ticker); interval
      configurable via `KATCHUP_SYNC_INTERVAL` (default `6h`, was hardcoded 15m).
      Coalesce window via `KATCHUP_COALESCE_WINDOW` (default `30s`).
- [x] S9.4 Token-auth: route registered on the same `APIAuth(cfg.APIToken, apiMux)`
      wrapper as S8, so it inherits bearer/`?token=` auth + fail-closed 503.
- [x] S9.5 Tests: concurrent triggers → one run; trigger during running sync →
      joins; trigger within coalesce window → no new run; scheduled + trigger
      don't double-run (internal/imap/trigger_test.go); handler 400/404/202-coalesce
      (internal/api/sync_trigger_test.go).

---

## Sprint S10: Meilisearch — Header-Only Search [DONE]
**Goal**: Full-text search over headers (to/from/subject), bodies stay
encrypted and un-indexed.

- [x] S10.1 `docker-compose.yml`: add a `meilisearch` service (internal network
      only, `MEILI_MASTER_KEY` set, own volume). Still NO Postgres, NO Redis.
      (getmeili/meilisearch:v1.10, no published host port, `meili_data` volume.)
- [x] S10.2 On message store, push a doc:
      `{id, account_id, folder, message_id, from, to, subject, date}` — **NO
      body, NO attachment text**. `internal/search.Doc` has no body field;
      `Store.InsertAndIndexMessage` pushes it best-effort after insert.
- [x] S10.3 `GET /search?q=` UI: query Meili, render results, link each to
      `/browse/download/{id}`. Filter by account. (`internal/api/search.go`,
      `internal/view/search`.)
- [x] S10.4 Config: `MEILI_URL`, `MEILI_KEY`. If unset (or Meili errors),
      `/search` degrades to `SearchMessagesLike` (DB LIKE over subject/from).
- [x] S10.5 Tests: index on store (injected fake indexer); LIKE fallback returns
      matching subjects/from; indexed Doc JSON has no body field. Pass w/o Meili.

---

## Sprint S11: Gmail API Ingest Driver (insurance) [DECIDED: STUB]
**Goal**: Belt-and-suspenders if Gmail ever drops IMAP. Not urgent — Gmail IMAP
is always-on. **Decision: ship as a documented stub** (interface + spec, no
implementation) until a Google Cloud OAuth client is provisioned.

- [ ] S11.1 Define a `MailSource` interface; make current IMAP sync one impl.
- [ ] S11.2 Gmail API impl: OAuth2 `gmail.readonly`, token storage + refresh,
      `history.list` for incremental. Requires a Google Cloud OAuth client.
- [ ] S11.3 Per-account `source` field (`imap` | `gmail_api`) selects the driver.
- [ ] S11.4 May ship as a documented STUB if OAuth client credentials aren't
      provisioned yet. Do not block S7–S10 on this.

---

## RESOLVED DECISIONS
- **S7.6 backfill**: NUKE + re-sync. (Inbox is real but disposable; re-reading
  is safe — `BODY.PEEK[]` means re-sync never marks live mail read.)
- **S8/S9 auth**: `KATCHUP_API_TOKEN`, fail-closed. Accept via bearer header OR
  `?token=` (Hermes header support unconfirmed — support both).
- **S10**: Add Meilisearch now (accepted second container).
- **S11**: Ship as a documented STUB (no OAuth client yet).

---

## Completed (v1)
- S1 Core — Account CRUD + DB
- S2 IMAP Sync Worker
- S3 Encryption Layer (master-key AES-256-GCM; PIV never built)
- S4 Web UI — Accounts
- S5 Web UI — Sync + Browse
- S6 Docker + Polish
