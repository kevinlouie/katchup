# Agent Build Instructions

## Prerequisites
- Go 1.26+
- sqlc (`go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest`)
- templ (`go install github.com/a-h/templ/cmd/templ@latest`) — note: the real
  templates are the hand-maintained `*_templ.go` files; `.templ` sources are
  stale decoys. `templ generate` is effectively unused; edit the Go directly.
- goose (`go install github.com/pressly/goose/v3/cmd/goose@latest`)
- (v2/S10) Meilisearch via docker-compose — no host install needed
- ~~YubiKey Manager (`ykman`)~~ — NOT used. PIV was never implemented.

## Project Setup
```bash
# Install Go dependencies
go mod init katchup
go mod tidy

# Generate sqlc code from SQL queries
sqlc generate

# Generate templ templates
templ generate

# Build the binary
CGO_ENABLED=0 go build -o bin/katchup ./cmd/katchup
```

## Running Tests
```bash
# Run all tests
go test ./...

# Run tests with verbose output
go test -v ./...

# Run tests for a specific package
go test -v ./internal/account/
go test -v ./internal/imap/
go test -v ./internal/crypto/
go test -v ./internal/api/

# Run tests with coverage
go test -coverprofile=coverage.out ./...
go tool cover -func=coverage.out
```

## Build Commands
```bash
# Development build
go build -o bin/katchup ./cmd/katchup

# Production build (stripped binary)
CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/katchup ./cmd/katchup
```

## Development Server
```bash
# Run with environment variables
DB_PATH=data/katchup.db KATCHUP_LISTEN=:8080 go run ./cmd/katchup

# Or use .env file
source .env && go run ./cmd/katchup
```

## Encryption Setup (master key)
No YubiKey. Encryption is master-key only:
```bash
# Generate a strong master key once, store it out of band (a password manager).
openssl rand -base64 32
# Provide it to the app:
export KATCHUP_MASTER_KEY='<that value>'
```
LOSING THIS KEY = all .eml.enc files and stored IMAP passwords are unrecoverable.

## Code Generation
```bash
# After modifying sql/queries/*.sql:
sqlc generate

# After modifying internal/view/**/*.templ:
templ generate

# After adding new sql/migrations/*.sql:
# Migrations run automatically on startup via goose embedded
```

## Docker
```bash
# Build Docker image
docker compose build

# Run with Docker
docker compose up -d

# View logs
docker compose logs -f

# Stop
docker compose down
```

## Makefile Targets
```bash
make build      # Build binary
make run        # Run locally
make test       # Run tests
make generate   # Run sqlc + templ generate
make docker     # Build + run Docker
make clean      # Remove build artifacts
```

## Key Learnings
- Always use CGO_ENABLED=0 — the SQLite driver is pure Go (modernc.org/sqlite)
- Staying on SQLite (WAL). NOT moving to Postgres — blobs are files on disk, DB
  is metadata-only; a few GB of mail is nothing for SQLite.
- sqlc-generated code lives in internal/database/ — never hand-edit EXCEPT the
  hand-added `MarkSyncRunProgress` in sync_run.sql.go (documented, keep in sync
  with sql/queries/sync_run.sql).
- The `.templ` sources are DEAD decoys — the shipped templates are the
  hand-maintained `*_templ.go` files (base/account/sync/browse). Edit those.
- Sync is non-mutating: `BODY.PEEK[]`, batched (200/batch, 120s), per-batch
  watermark + `MarkSyncRunProgress`. Never STORE/APPEND/EXPUNGE.
- (v2/S7) Message index + content-hash dedup. Migration 003 adds `blobs`
  (UNIQUE(account_id,sha256), refcount) and `messages` (UNIQUE(account_id,folder,
  uid)). Sync computes `sha256(raw RFC822)`; an existing blob is reused (refcount
  bumped, file NOT rewritten), otherwise the `.eml.enc` is written and a blob
  inserted. `blobs.path` is RELATIVE to the data dir. `messages` stores
  Message-ID, fuzzy_fp = `sha256(lower(from)|internal_date|lower(subject))`,
  from/to/subject/internal_date/size. `fetchAndWriteMessage` is idempotent via a
  `MessageExists` guard (an aborted batch is retried next run — do not double-bump
  refcount). Browse (`/browse`) lists from `messages JOIN blobs`; download is
  `GET /browse/download/{id}` and resolves `blob.path`. Queries in
  `sql/queries/message.sql`; store wrappers in `internal/imap/store.go`.
- (v2/S7.6) BACKFILL = NUKE + RE-SYNC. There is no migration for pre-S7 on-disk
  `.eml.enc` files into `blobs`/`messages`. On the NAS: stop the container, delete
  the `data/<account_id>/` mail dirs AND remove the DB (or just the mail; a fresh
  DB is simplest), restart, let the scheduler re-sync. Safe because `BODY.PEEK[]`
  never marks live mail read. Do NOT write a backfill script; do NOT delete
  `data/` from code.
- Encryption is master-key AES-256-GCM (`FormatMasterKey = 0x02`). PIV
  (`FormatRSA = 0x01`) is a never-built stub. `account_encryption` is vestigial.
- Dashboard is served at `GET /{$}` (root), not `/sync`.
- v2 API (`/api/*`) is Hermes-facing: bearer `KATCHUP_API_TOKEN`, fail-closed
  (503 if unset). `/api/archived` (pull, Message-ID keyed), `/api/sync` (async
  202, per-account mutex + coalesce).
- (v2/S8) Archived-lookup API (Hermes pull side), wired in main.go under a single
  `APIAuth(cfg.APIToken, apiMux)` wrapper so EVERY `/api/*` route (even undefined
  ones) fails closed with 503 when the token is unset. Token accepted via
  `Authorization: Bearer <token>` OR `?token=<token>` (constant-time compare).
  `GET /api/archived?message_id=<id>[&fp=<fuzzy_fp>]` → `{archived, archived_at,
  id, sha256}`; matches `messages.message_id_hdr` first, falls back to
  `messages.fuzzy_fp` only when `fp=` is supplied AND the Message-ID missed.
  `POST /api/archived/lookup {message_ids:[...]}` → map of message-id → status.
  Newest match wins (`ORDER BY m.id DESC LIMIT 1`) since a Message-ID can appear
  in multiple folders/accounts. Queries `GetArchivedByMessageID` /
  `GetArchivedByFuzzyFp` in message.sql; store wrappers `LookupArchivedByMessageID`
  / `LookupArchivedByFuzzyFp` in imap/store.go; handler in internal/api/archived.go.
- (v2/S8.4) OPERATIONAL: sync Gmail's **All Mail** folder (not just INBOX) so
  Hermes label/archive moves never hide an already-unarchived message from
  katchup's index — otherwise the archived-lookup could false-negative on mail
  Gmail moved out of INBOX.
- (v2/S9) On-demand sync trigger + per-account mutex. `Syncer.Run` was refactored
  into `lockFor` (per-account exec mutex) + `beginRun` (clear stale, DB running-
  check, CreateSyncRun) + `executeSync` (the folder loop, now behind the `runBody`
  func field so tests can stub IMAP). Run creation ONLY happens while holding the
  exec lock, so scheduled `SyncAll` and the trigger can never double-create a run.
  `Syncer.TriggerSync(ctx, accountID, coalesceWindow)` is the on-demand entry:
  TryLock the exec lock; success → coalesce onto a run finished < window ago
  (`recentRunWithinWindow`, newest run via ListRecentRuns[0]) or create a run and
  run it in a background goroutine that unlocks on finish; TryLock failure → a sync
  is in flight, join it (`awaitRunningRun` briefly polls GetCurrentSyncRun to bridge
  the create window). `POST /api/sync?account=<id>` (internal/api/sync_trigger.go)
  returns 202 `{run_id, account_id, started}`; started=false when coalesced/joined.
  Registered on the S8 `APIAuth` apiMux so it inherits token auth + 503 fail-closed.
  Scheduled ticker is a SAFETY-NET FLOOR (do not disable), interval
  KATCHUP_SYNC_INTERVAL (default 6h). Coalesce window KATCHUP_COALESCE_WINDOW
  (default 30s). Sync run's finished_at is datetime('now') (UTC); coalesce parses
  it with time.DateTime and compares via time.Since (absolute instants, tz-safe).
- Meilisearch (v2/S10) indexes HEADERS ONLY — never message bodies. Implemented:
  `internal/search` package holds `Doc` (id/account_id/folder/message_id/from/to/
  subject/date — NO body field, structurally impossible to index a body),
  `Indexer`/`Searcher` interfaces, `NoopIndexer` (default), and `Meili` (net/http
  client, no SDK dep). The imap `Store` holds an `indexer` (default NoopIndexer,
  swapped via `SetIndexer` when MEILI_URL is set). Sync calls
  `Store.InsertAndIndexMessage` (was `InsertMessage`): it inserts the row, resolves
  messages.id via `GetMessageIDByUID`, then pushes a header-only `Doc`. Indexing is
  BEST-EFFORT — a Meili failure is logged + swallowed so it never fails the sync
  (the message is already durably stored); the noop path skips the id lookup.
  `GET /search?q=&account=` (`internal/api/search.go`) prefers Meili, falls back to
  the SQLite LIKE (`SearchMessagesLike` over subject/from ONLY) when MEILI_URL is
  unset OR Meili errors — search is never a hard dependency. Results link to
  `/browse/download/{id}`. View is `internal/view/search/page_templ.go` (hand-
  maintained Go, like the others); nav gained a "Search" link (active="search").
  Config: MEILI_URL / MEILI_KEY. docker-compose adds a `meilisearch` service
  (getmeili/meilisearch:v1.10, internal-only — NO published host port, own
  `meili_data` volume, MEILI_MASTER_KEY from env). `Meili.EnsureIndex` (called at
  startup, best-effort) creates the "messages" index (primaryKey id) and marks
  account_id/folder filterable. Tests: header-only Doc JSON (no body key), indexing
  invoked on store via injected fake indexer, LIKE fallback matches subject/from,
  Meili-preferred + error-fallback — all pass WITHOUT a running Meili.
- Goose migrations run on startup via filepath.Join("sql", "migrations") — Dockerfile copies migrations to /app/sql/migrations/
- Standard net/http ServeMux for routing (no third-party router)
- YubiKey PIV operations: plaintext never leaves the container, keys never exported
- .eml files are stored in data/ — this directory is gitignored
- All timestamps stored as UTC in the database
- KATCHUP_MASTER_KEY env var required for encryption — set it for production, optional for dev
- KATCHUP_ENV=production enables JSON logging via log/slog; default is "development" (text logs)
- (v2) KATCHUP_API_TOKEN — auth for /api/* via bearer header OR ?token= query param (support both); if unset, /api/* is 503 (fail closed)
- (v2) KATCHUP_SYNC_INTERVAL — scheduled floor interval (default 6h); Hermes trigger drives freshness on top
- (v2) MEILI_URL / MEILI_KEY — Meilisearch endpoint + key; if unset, /search falls back to SQLite LIKE
- (v2) KATCHUP_THROTTLE_COOLDOWN — after a provider throttle (Gmail bandwidth/OVERQUOTA/too-many-connections), the run is marked status="throttled" and the account is SKIPPED (scheduled + trigger) until cooldown elapses (default 24h; 0 disables). Detection = isThrottleError substring match in imap/sync.go; guard = throttledUntil() reading ListRecentRuns[0].
- (v2) KATCHUP_FETCH_PACING — optional inter-batch delay (default 0). Eases connection/rate limits; does NOT reduce bytes/day so won't prevent a bandwidth-cap throttle.
- Subcommands: `katchup backfill` (rebuild blobs/messages/search + watermark from on-disk .eml.enc, no IMAP) and `katchup reindex` (push existing message headers to Meili, no decrypt). ENTRYPOINT is /app/katchup so `docker run ... backfill` / `docker exec <c> /app/katchup reindex`.
- GET /health returns {"status":"ok"} for Docker HEALTHCHECK
- Encrypted files use .eml.enc extension with version-prefixed binary format
- Account store requires master key: account.New(db, masterKey)
- Syncer requires key wrapper: imap.NewSyncer(store, dataDir, keyWrapper, encStore)
- Download endpoint pattern: GET /download/{accountID}/{date}/{filename}
- Crypto package uses KeyWrapper interface — MasterKeyWrapper for dev, pluggable for YubiKey
