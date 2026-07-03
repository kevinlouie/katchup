# Ralph Fix Plan — Katchup

## IMPORTANT CONTEXT
Katchup is a new project. No sprints complete yet.

Follow the sprint order: S1 → S2 → S3 → S4 → S5 → S6.
Do not skip ahead — each sprint builds on the previous one.

---

## Sprint S1: Core — Account CRUD + DB [COMPLETE]
**Goal**: Database schema, account model, sqlc queries. Foundation for everything else.

- [x] S1.1 Migration `001_initial.sql` — `accounts` table + `sync_runs` table
- [x] S1.2 `sql/queries/account.sql` — CreateAccount, GetAccount, ListAccounts, UpdateAccount, DeleteAccount
- [x] S1.3 `sql/queries/sync_run.sql` — CreateSyncRun, UpdateSyncRunStatus, ListRecentRuns
- [x] S1.4 `sqlc generate` — `internal/database/account.sql.go` + `sync_run.sql.go`
- [x] S1.5 `internal/config/config.go` — DB_PATH, KATCHUP_LISTEN env vars
- [x] S1.6 `internal/account/store.go` — SQLite-backed account store (connect DB, run migrations, CRUD)
- [x] S1.7 `internal/account/store_test.go` — Create/List/Get/Update/Delete tests
- [x] S1.8 `go build ./...` clean

**Tests:**
| Function | Test | Type |
|----------|------|------|
| `CreateAccount` | inserts row, returns ID | Integration |
| `GetAccount` | returns correct row by ID | Integration |
| `ListAccounts` | returns all accounts | Integration |
| `UpdateAccount` | modifies fields correctly | Integration |
| `DeleteAccount` | removes row + cascades sync_runs | Integration |

---

## Sprint S2: IMAP Sync Worker [COMPLETE]
**Goal**: Connect to IMAP, fetch emails, write .eml files, track sync state.

- [x] S2.1 `internal/imap/sync.go` — `Syncer` struct with IMAP config, `Run(ctx)` method
- [x] S2.2 IMAP connection: dial with TLS, login, select INBOX (and optionally other folders)
- [x] S2.3 Fetch unseen messages: `UID SEARCH UNSEEN`, fetch headers + body as RFC822
- [x] S2.4 Write `.eml` files: `data/{account_id}/{folder}/{timestamp}_{uid}.eml`
- [x] S2.5 Dedup: skip UIDs already present in `data/`
- [x] S2.6 `internal/imap/store.go` — persist `last_uid`, `last_sync_at`, `errors` per account
- [x] S2.7 Integration in `cmd/katchup/main.go` — scheduled run loop (ticker every 15 min)
- [x] S2.8 `go build ./...` clean; basic sync test against test server

**Tests:**
| Function | Test | Type |
|----------|------|------|
| `writeEML` | writes file with correct path format | Unit |
| `emlPath` | generates correct file path | Unit |
| `GetLastSyncState` | returns last completed sync UID | Integration |
| `GetCurrentSyncRun` | returns running sync run or nil | Integration |

---

## Sprint S3: Encryption Layer [COMPLETE]
**Goal**: YubiKey PIV encryption/decryption for .eml files at rest.

- [x] S3.1 `internal/crypto/yubikey.go` — `KeyWrapper` interface, `MasterKeyWrapper` struct, AES-256-GCM encrypt/decrypt
- [x] S3.2 RSA-2048 key wrap support: `KeyWrapper` interface designed for YubiKey PIV (slot 9a); `MasterKeyWrapper` uses AES-GCM key wrap for dev mode
- [x] S3.3 AES-256-GCM encrypt/decrypt: content key wraps/ unwraps the file
- [x] S3.4 Store encryption metadata in DB: `account_encryption` table (slot_id, encrypted_content_key_prefix) + SQL queries + store methods
- [x] S3.5 Integrate into sync: write encrypted .eml.enc, update metadata, encrypt stored passwords
- [x] S3.6 Add decrypt endpoint: `GET /download/{accountID}/{date}/{filename}` — fetch, decrypt, serve .eml
- [x] S3.7 `go build ./...` clean; all tests passing

**Tests:**
| Function | Test | Type |
|----------|------|------|
| `wrapContentKey`/`unwrap` | round-trip preserves 32-byte key | Unit |
| `encryptFile`/`decryptFile` | round-trip preserves .eml content | Unit |
| Encrypt large files (100KB) | preserves content integrity | Unit |
| Decrypt wrong key | returns error | Unit |
| Decrypt tampered file | returns error (GCM auth tag) | Unit |
| `encryptPassword`/`decryptPassword` | round-trip preserves password | Unit |
| `EncryptedKeyPrefix`/`MatchPrefix` | correct hex prefix extraction | Unit |

---

## Sprint S4: Web UI — Accounts [COMPLETE]
**Goal**: Account management UI — list, add, edit, delete accounts.

- [x] S4.1 `internal/view/account/page.templ` — account list page + add/edit forms
- [x] S4.2 `internal/api/account.go` — HTTP handlers for account CRUD
- [x] S4.3 Routes: `GET /accounts`, `POST /accounts/new`, `GET /accounts/{id}/edit`, `POST /accounts/{id}/edit`, `POST /accounts/{id}/delete`
- [x] S4.4 Password field: encrypted storage (use encryption module or simple env-key AES for stored credentials)
- [x] S4.5 Nav link "Accounts" added to `internal/view/layout/base.templ`
- [x] S4.6 `templ generate` + `go build ./...` clean

**Tests:**
| Function | Test | Type |
|----------|------|------|
| Account handlers | CRUD returns correct status codes + redirects | HTTP |
| Add form | validates required fields | HTTP |

---

## Sprint S5: Web UI — Sync + Browse [COMPLETE]
**Goal**: Sync status dashboard, manual trigger, email browser.

- [x] S5.1 `internal/view/sync/page.templ` — sync status per account + trigger sync button
- [x] S5.2 `internal/view/browse/page.templ` — search/browse backed-up emails (list by date/account)
- [x] S5.3 `GET /sync` — sync dashboard handler
- [x] S5.4 `POST /sync/{account_id}/trigger` — manual sync trigger (async goroutine)
- [x] S5.5 `GET /browse` — browse handler (list .eml files, paginate)
- [x] S5.6 `GET /browse/{account_id}/{folder}/{filename}` — download decrypted .eml
- [x] S5.7 Nav link "Sync" + "Browse" added to layout
- [x] S5.8 `templ generate` + `go build ./...` clean

**Tests:**
| Function | Test | Type |
|----------|------|------|
| Sync handler | triggers sync, returns 303 | HTTP |
| Browse handler | lists files, paginates | HTTP |
| Browse download | serves email file | HTTP |
| ParseEMLFilename | extracts date/UID from filename | Unit |
| FormatSize | formats byte count | Unit |

---

## Sprint S6: Docker + Polish [COMPLETE]
**Goal**: Production deployment with Docker, logging, health checks.

- [x] S6.1 `Dockerfile` — multi-stage Alpine build
- [x] S6.2 `docker-compose.yml` — service with volumes for data/
- [x] S6.3 Non-root user in Docker
- [x] S6.4 HEALTHCHECK with wget to `/health`
- [x] S6.5 `log/slog` JSON handler for production (text for dev)
- [x] S6.6 `GET /health` endpoint returning JSON
- [x] S6.7 `.dockerignore` + `.gitignore` (data/, *.eml)

---

## Completed
- Sprint S1: Core — Account CRUD + DB
- Sprint S2: IMAP Sync Worker
- Sprint S3: Encryption Layer
- Sprint S4: Web UI — Accounts
- Sprint S5: Web UI — Sync + Browse
- Sprint S6: Docker + Polish
