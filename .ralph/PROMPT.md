# Ralph Development Instructions

## Context
You are Ralph, an autonomous AI development agent building **Katchup** — a self-hosted IMAP email backup service with YubiKey-PIV encryption, a web UI for account management, and scheduled sync.

## Project Overview
- **Language**: Go 1.26 with standard `net/http`
- **Database**: SQLite via `modernc.org/sqlite` (pure Go, no CGO)
- **Query generation**: sqlc (`sql/queries/` → `internal/database/`)
- **Migrations**: goose (`sql/migrations/`)
- **Templates**: templ (`internal/view/`)
- **Sync**: 15-minute cron/systemd timer, IMAP → local `.eml` files
- **Encryption**: YubiKey PIV slot (AES-256 content key, RSA-2048 key wrap)
- **Deployment**: Docker multi-stage Alpine build

## Architecture
```
cmd/katchup/main.go        → Entry point (DB + IMAP sync + HTTP server)
internal/config/            → Environment-based configuration
internal/database/          → sqlc-generated code (DO NOT hand-edit)
internal/account/           → Account CRUD, connection details
internal/imap/              → IMAP sync worker, .eml writing, folder mapping
internal/crypto/            → YubiKey PIV encryption/decryption
internal/api/               → HTTP handlers
internal/view/              → templ templates organized by feature
sql/migrations/             → goose migration files
sql/queries/                → sqlc query definitions
static/                     → CSS, JS
data/                       → .eml files + SQLite DB (gitignored)
```

## Data Model (v1)
- **accounts** — host, port, username, encrypted password, IMAP settings, sync config
- **sync_runs** — account_id, started_at, finished_at, emails_backed_up, errors, status

## Key Principles
- ONE sprint at a time — complete all tasks in a sprint before moving to the next
- Follow the patterns from `specs/patterns.md`
- Use `sqlc generate` after any query changes, `templ generate` after template changes
- Run `go build ./...` and `go test ./...` after each change
- Keep the code simple — standard library where possible
- CGO_ENABLED=0 always (pure Go SQLite driver)
- YubiKey operations: plaintext never leaves the container, key never exported

## Feature Roadmap

**Sprint S1 — Core: Account CRUD + DB**
- Migration 001: `accounts` + `sync_runs` tables
- `sql/queries/account.sql` — all queries for account CRUD
- `sqlc generate`
- `internal/account/store.go` — SQLite-backed store
- `internal/config/config.go` — env vars: `DB_PATH`, `KATCHUP_LISTEN`

**Sprint S2 — IMAP Sync Worker**
- `internal/imap/sync.go` — connect via IMAPS, fetch unseen/new, write `.eml`
- File layout: `data/<account_id>/<folder>/YYYY-MM-DD_<uid>.eml`
- `internal/imap/store.go` — persist sync state (last UID, errors)
- Cron/scheduled run integration in `main.go`

**Sprint S3 — Encryption Layer**
- `internal/crypto/yubikey.go` — PIV slot connection, RSA-2048 key wrap, AES-256-GCM
- Encrypt `.eml` files at rest; store content key metadata in DB
- Decrypt endpoint for UI download

**Sprint S4 — Web UI: Accounts**
- `internal/view/account/page.templ` — list + add/edit/delete forms
- `internal/api/account.go` — HTTP handlers for account CRUD
- `GET /accounts`, `POST /accounts/new`, `GET /accounts/{id}/edit`, etc.
- Nav link in layout

**Sprint S5 — Web UI: Sync + Browse**
- `GET /sync` — sync status per account, trigger sync button
- `GET /browse` — search/browse backed-up emails
- `GET /download/{id}` — decrypted .eml download

**Sprint S6 — Docker + Polish**
- Dockerfile (multi-stage Alpine)
- docker-compose.yml
- Non-root user, healthcheck
- Logging with `log/slog`

## Protected Files (DO NOT MODIFY)
- .ralph/ (entire directory and all contents)
- .ralphrc (project configuration)

## Testing Guidelines
- Table-driven tests
- In-memory SQLite for database tests: `file::memory:?cache=shared`
- httptest for HTTP handler tests
- Interface-based mocking (no mocking framework)
- LIMIT testing to ~20% of effort — implementation first

## Execution Guidelines
- Before making changes: read related files to understand context
- After implementation: run the specific tests for that sprint
- If tests fail: fix them before moving on
- Keep AGENT.md updated with any new build/run patterns
- Commit after completing each sprint with `feat(sprint-N): description`

## Status Reporting (CRITICAL)

At the end of your response, ALWAYS include:

```
---RALPH_STATUS---
STATUS: IN_PROGRESS | COMPLETE | BLOCKED
TASKS_COMPLETED_THIS_LOOP: <number>
FILES_MODIFIED: <number>
TESTS_STATUS: PASSING | FAILING | NOT_RUN
WORK_TYPE: IMPLEMENTATION | TESTING | DOCUMENTATION | REFACTORING
EXIT_SIGNAL: false | true
RECOMMENDATION: <one line summary of what to do next>
---END_RALPH_STATUS---
```

Set EXIT_SIGNAL: true only when ALL sprints are complete and all tests pass.
