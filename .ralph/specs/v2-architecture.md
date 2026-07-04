# v2 Architecture — Dedup, Hermes API, Search, Gmail

Design decisions for katchup v2. Supersedes stale bits of the older specs.

## Guiding property: single stateful service
katchup stays a **single Go binary + SQLite**. No Postgres, no Redis.
- Email blobs are encrypted files on disk; the DB holds **metadata only**.
  Even 500k messages ≈ tens of MB of rows — SQLite (WAL) handles it trivially.
- Postgres only earns its keep with concurrent writers or LISTEN/NOTIFY.
  katchup's sync is single-writer append-only; nothing here needs it.
- The ONE allowed exception is a **Meilisearch** container for search (S10).
  It has its own store and does not touch SQLite.

## Content-hash dedup (S7)
- Dedup key = `SHA-256(raw RFC822 bytes)`, scoped **per-account** (cleaner for
  per-account retention/delete later; global would couple accounts' blobs).
- `blobs` is unique on `(account_id, sha256)` with a `refcount`. `messages` is
  unique on `(account_id, folder, uid)` and points at a blob.
- A message appearing in multiple folders (Gmail Inbox + All Mail, or a Hermes
  label/move) creates a new `messages` row but reuses the existing blob
  (refcount++). No double-store.

## Hermes integration
Hermes (nousresearch.com Hermes agent) plugs **directly into the live mailbox**
and triages mail live. katchup is the insurance layer. The rule: **Hermes must
not touch a message until katchup has archived it.**

### Correlation key = RFC `Message-ID` header
- Gmail API `message.id`, IMAP `X-GM-MSGID`, and katchup's UID are three
  different numbers for the same mail — useless across systems.
- The `Message-ID:` header is the one value both sides always see (Gmail API
  returns it in `payload.headers`; IMAP returns it; katchup parses it).
- Fallback for missing/garbage Message-ID: `fuzzy_fp =
  sha256(lower(from)|internal_date|lower(subject))`.

### Delivery = pull, not push
Hermes already holds + retries, so katchup exposes a read-only "is it archived?"
query and Hermes polls until true. No webhook/HMAC/retry queue on katchup's side.
```
GET  /api/archived?message_id=<id>        -> {archived, archived_at, id, sha256}
POST /api/archived/lookup {message_ids[]}  -> batch map
```

### On-demand trigger (S9) with anti-stampede
```
POST /api/sync?account=<id>  -> 202 {run_id}   (async; never blocks on the sync)
```
- **Per-account mutex**: at most one running sync per account. A trigger during
  a running sync JOINS it (returns the running run id), never starts a second.
- **Coalesce window** (default 30s): a trigger right after a sync just finished
  returns that run, no redundant scan.
- **Keep the scheduled sync as a floor** (default 6h). Do NOT let Hermes be the
  sole driver — if Hermes is down, the backup must not silently stop.

### Sync All Mail (Gmail)
Hermes labels/moves live mail. Gmail's **All Mail** retains everything
regardless of labels, so syncing All Mail guarantees a Hermes move can never
hide an unarchived message from katchup.

### API auth
All `/api/*` routes require the token, accepted via EITHER
`Authorization: Bearer <KATCHUP_API_TOKEN>` OR `?token=<KATCHUP_API_TOKEN>`
(Hermes's header support is unconfirmed, so support both; query-param token is
acceptable on LAN). Fail closed: if the env var is unset, `/api/*` returns 503.

## Search (S10) — headers only
- Index `{from, to, subject, message_id, date, folder, account}` into Meili.
- **Bodies stay encrypted and are NOT indexed** (accepted: header metadata in
  Meili plaintext is fine on LAN; message bodies must remain encrypted at rest).
- If Meili is unconfigured, `/search` degrades to a SQLite `LIKE` over
  subject/from — no hard dependency.

## Gmail API fallback (S11) — insurance
- `MailSource` interface; IMAP is one impl, Gmail API another (OAuth2
  `gmail.readonly`, `history.list` incremental).
- Lowest priority; Gmail IMAP is always-on. May ship as a stub until a Google
  Cloud OAuth client is provisioned.
