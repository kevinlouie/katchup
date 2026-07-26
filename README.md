# 🥫 katchup

Self-hosted, **non-mutating** IMAP email backup. katchup connects to your
mailboxes read-only, archives every message as an encrypted file on disk, and
indexes it in SQLite — a single Go binary you can run on a NAS and forget about.

It is deliberately *not* an email client and *not* a compliance platform. It is
insurance: a private, encrypted, append-only copy of your mail that never
touches the live account.

## Why it exists

- **Non-mutating.** Fetches with `BODY.PEEK[]` — never sets `\Seen`, never
  `STORE`/`APPEND`/`EXPUNGE`. Backing up your mail can't change it. Deleting or
  re-labelling mail on the server never deletes it here.
- **Encrypted at rest.** Every message is AES-256-GCM encrypted before it hits
  disk. Lose the server, keep your privacy.
- **Single stateful service.** One Go binary + SQLite. Message *blobs* are
  encrypted files on disk; the database holds metadata only. No Postgres, no
  Redis. (Search adds one optional Meilisearch container — see below.)
- **Deduplicated.** Identical messages (e.g. Gmail's Inbox + All Mail) are
  stored once and reference-counted.
- **Automatable.** A token-guarded HTTP API lets an external agent trigger a
  sync and ask "is this message archived yet?" — so it can act on mail only
  after katchup has a safe copy.

## Architecture

| Concern | Choice |
|---|---|
| Language | Go 1.26, standard `net/http` (no framework) |
| Database | SQLite (`modernc.org/sqlite`, pure Go, `CGO_ENABLED=0`) |
| Migrations | goose, embedded in the binary |
| Queries | sqlc-generated |
| IMAP | `github.com/emersion/go-imap` v1 |
| Search | Meilisearch (optional; **headers only**), stdlib HTTP client |
| Encryption | AES-256-GCM, per-file content key wrapped by a master key |
| UI | Server-rendered templates, Tailwind + fonts vendored & embedded (no CDN, works offline), dark theme |
| Deploy | Multi-stage Alpine Docker image, non-root, healthcheck |

### Storage layout
```
data/
├── katchup.db                                  # SQLite (metadata + message index)
└── <account_id>/<folder>/<YYYY-MM-DD>_<uid>.eml.enc   # encrypted message blobs
```

## Quick start (Docker)

```bash
# 1. Secrets — generate strong random values, store them somewhere safe.
#    (.env is gitignored; keep it out of any repo regardless.)
cat > .env <<EOF
KATCHUP_MASTER_KEY=$(openssl rand -hex 32)   # encrypts all mail — see warning below
MEILI_MASTER_KEY=$(openssl rand -hex 32)     # required if you run search
KATCHUP_API_TOKEN=$(openssl rand -hex 32)    # enables the /api/* automation surface
KATCHUP_UI_KEY=$(openssl rand -hex 16)       # web-UI access key (or set one on first visit)
EOF

# 2. Bring it up (katchup + meilisearch).
docker compose pull
docker compose up -d

# 3. Add a mailbox in the web UI, then let it sync.
open http://localhost:8080
```

> ⚠️ **Back up `KATCHUP_MASTER_KEY` out of band.** It is derived (SHA-256) into
> the key that unwraps every message and every stored IMAP password. **Lose it
> and every `.eml.enc` file is permanently unrecoverable.** It is not stored in
> the database.

## Configuration

All configuration is environment variables — no config files.

| Variable | Default | Purpose |
|---|---|---|
| `DB_PATH` | `data/katchup.db` | SQLite database path |
| `KATCHUP_LISTEN` | `:8080` | HTTP listen address |
| `KATCHUP_ENV` | `development` | `production` enables JSON structured logging |
| `KATCHUP_MASTER_KEY` | — | **Required for encryption.** Wraps per-file content keys. ⚠️ Unset ⇒ mail **and IMAP passwords** are stored in plaintext |
| `KATCHUP_UI_KEY` | — | Web-UI access key. Unset ⇒ the UI asks you to create one on first visit (hash stored in the DB) |
| `KATCHUP_API_TOKEN` | — | Bearer token for `/api/*`. **Unset ⇒ `/api/*` returns 503** (fail closed) |
| `KATCHUP_SYNC_INTERVAL` | `6h` | Scheduled-sync safety-net floor |
| `KATCHUP_COALESCE_WINDOW` | `30s` | Ignore a trigger this soon after a run finished |
| `MEILI_URL` | — | Meilisearch endpoint. Unset ⇒ `/search` falls back to a SQLite `LIKE` |
| `MEILI_KEY` | — | Meilisearch API key |

## Web routes

All web routes (everything except `/health` and `/api/*`) require login.

| Path | Description |
|---|---|
| `GET /login`, `POST /logout` | Unlock / lock the UI (first visit: create the access key) |
| `GET /` | Dashboard — accounts, sync status, totals |
| `GET /accounts`, `…/new`, `…/{id}/edit`, `…/{id}/delete` | Mailbox management |
| `GET /sync`, `POST /sync/{id}/trigger` | Sync status + manual trigger (UI) |
| `GET /browse` | Browse archived mail (from the DB index) |
| `GET /browse/download/{id}` | Download a decrypted `.eml` |
| `GET /search?q=` | Header-only full-text search |
| `GET /health` | JSON healthcheck |

## Automation API (`/api/*`)

Machine-facing endpoints for an external triage agent. **All require a token**,
supplied as `Authorization: Bearer <KATCHUP_API_TOKEN>` (query-param tokens are
not accepted — URLs end up in logs). If `KATCHUP_API_TOKEN` is unset, every
`/api/*` route returns **503** — it never silently opens.

| Endpoint | Description |
|---|---|
| `GET /api/archived?message_id=<id>[&fp=<fuzzy_fp>]` | Is this message archived? Returns `{archived, archived_at, id, sha256}` |
| `POST /api/archived/lookup` `{message_ids:[…]}` | Batch archived-status lookup |
| `POST /api/sync?account=<id>` | Trigger a sync. Returns **202** with a run id; runs async, coalesces onto a recent/in-flight run |

### Integration pattern

The archived-lookup is keyed on the RFC 5322 **`Message-ID`** header — the one
identifier a client sees identically over IMAP and the Gmail API (a fuzzy
`from|date|subject` fingerprint is the fallback). A triage agent that reads the
live mailbox can:

1. `POST /api/sync?account=<id>` to nudge a fresh backup.
2. Poll `GET /api/archived?message_id=<id>` until `archived: true`.
3. Only then act on the message — guaranteeing it never touches mail katchup
   hasn't safely copied.

For Gmail, sync **All Mail**: it retains every message regardless of labels, so
an agent moving/labelling live mail can never hide an unarchived message from
katchup's index.

## Search

`/search` queries Meilisearch and indexes **headers only** — `from`, `to`,
`subject`, `message_id`, `date`, `folder`, `account`. **Message bodies are never
indexed**; they stay in the encrypted blobs. If `MEILI_URL` is unset, search
degrades gracefully to a SQLite `LIKE` over subject/from with no external
dependency.

## Development

```bash
# Prerequisites: Go 1.26+, sqlc, goose. (No YubiKey, no templ workflow needed —
# the shipped templates are hand-maintained *_templ.go files.)

sqlc generate                                   # after editing sql/queries/*.sql
CGO_ENABLED=0 go build ./...
go test ./...

# Run locally
DB_PATH=data/katchup.db KATCHUP_LISTEN=:8080 KATCHUP_MASTER_KEY=dev-key \
  go run ./cmd/katchup
```

Migrations in `sql/migrations/` run automatically on startup (embedded goose).
sqlc-generated code lives in `internal/database/` — don't hand-edit it.

## Security posture

- Designed for a **trusted LAN** (e.g. behind Tailscale). Even so, do **not**
  port-forward it to the internet.
- The web UI requires an access key (`KATCHUP_UI_KEY`, or created on first
  visit). The session cookie is `HttpOnly` + `SameSite=Lax`, which also blocks
  cross-site request forgery against the state-changing routes.
- The `/api/*` surface is token-guarded (Bearer header only) and fails closed.
- **Set `KATCHUP_MASTER_KEY`, and make it a random value** (e.g.
  `openssl rand -hex 32`). It is stretched with a plain SHA-256, not a
  password KDF — a short human passphrase weakens every blob. Without a master
  key, mail *and* stored IMAP passwords are written in plaintext.
- Plaintext and content keys never leave the process; content keys are
  ephemeral per file.

## License

[MIT](LICENSE). Vendored UI assets carry their own licenses: Tailwind CSS
(MIT), Bricolage Grotesque / Instrument Sans / JetBrains Mono (SIL OFL 1.1) —
see `internal/view/static/assets/`.
