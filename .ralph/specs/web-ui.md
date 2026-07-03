# Web UI Specification

## Stack
- **Templates**: templ (compiled Go templates)
- **CSS**: Tailwind CSS via CDN
- **Interactivity**: Vanilla JS for sync trigger, search
- **Routing**: Standard `net/http.ServeMux` with Go 1.22+ pattern matching

## Routes

| Method | Path | Handler | Description |
|--------|------|---------|-------------|
| GET | `/` | dashboard.Index | Dashboard: overview of all accounts + recent syncs |
| GET | `/health` | health.Check | JSON health status |
| GET | `/accounts` | account.List | All accounts |
| GET | `/accounts/new` | account.NewForm | Add account form |
| POST | `/accounts/new` | account.Create | Handle create |
| GET | `/accounts/{id}/edit` | account.EditForm | Edit account form |
| POST | `/accounts/{id}/edit` | account.Update | Handle update |
| POST | `/accounts/{id}/delete` | account.Delete | Delete account + cascade |
| GET | `/sync` | sync.Status | Sync status per account + trigger |
| POST | `/sync/{account_id}/trigger` | sync.Trigger | Manual sync trigger |
| GET | `/sync/{account_id}/logs` | sync.Logs | Recent sync logs for account |
| GET | `/browse` | browse.List | Browse backed-up emails (by account + date) |
| GET | `/browse/{account_id}/{date}/{filename}` | browse.Download | Download decrypted .eml |

## Layout

```
┌─────────────────────────────────────────────────┐
│ Katchup                                         │
├──────────┬──────────────────────────────────────┤
│ Accounts │  Dashboard Content                   │
│ Sync     │                                      │
│ Browse   │  ...                                 │
└──────────┴──────────────────────────────────────┘
```

- Sidebar navigation on left (Accounts, Sync, Browse)
- Main content area on right
- Active page highlighted in sidebar

## Dashboard (`/`)

- **Account summary**: count of accounts, count of backed-up emails total
- **Recent syncs**: last 5 sync runs across all accounts (account name, status, timestamp, emails backed up)
- **Account status cards**: per account — name, last synced, next sync, status indicator (green/yellow/red)

## Accounts Page (`/accounts`)

### List View
Table of all accounts:
| Name | Host | Username | Folders | Last Sync | Status | Actions |
|------|------|----------|---------|-----------|--------|---------|
| Personal | imap.gmail.com | me@gmail.com | INBOX | 2 min ago | Running | Edit, Delete |
| Work | mail.company.com | me@company.com | INBOX, Sent | 1 hr ago | Completed | Edit, Delete |

- **Add Account** button → redirects to `/accounts/new`
- **Edit** → redirects to `/accounts/{id}/edit`
- **Delete** → POST with confirmation

### Add/Edit Form (`/accounts/new`, `/accounts/{id}/edit`)

Fields:
- **Name** (text, required) — display name for the account
- **Host** (text, required) — e.g., `imap.gmail.com`
- **Port** (number, default 993)
- **Username** (text, required) — email address
- **Password** (password, required) — encrypted at rest
- **Connection** (radio): IMAPS (port 993) / STARTTLS (port 143)
- **Folders** (text, optional) — comma-separated folder names (default: INBOX)
- **YubiKey Slot** (select): 9a (primary), 9b, 9c, 9d

Buttons: Save / Cancel

## Sync Page (`/sync`)

### Sync Status Table
| Account | Last Sync | Status | Emails | Actions |
|---------|-----------|--------|--------|---------|
| Personal | 2 min ago | Running | 15 | Stop |
| Work | 1 hr ago | Completed | 142 | Trigger Sync |

- **Trigger Sync** button → POST `/sync/{account_id}/trigger`
- Auto-refresh every 30s via JS (or server-sent events in future)
- Running syncs show a spinner

### Sync Logs (`/sync/{account_id}/logs`)
List of recent sync runs for the account:
| Started | Finished | Status | Emails | Errors |
|---------|----------|--------|--------|--------|
| 2026-07-01 12:00 | 2026-07-01 12:00:15 | Completed | 15 | — |
| 2026-07-01 11:45 | — | Running | 8 | — |

## Browse Page (`/browse`)

### Account Filter
Dropdown to filter by account. Shows all accounts with email counts.

### Date Navigator
Calendar-style date picker to browse by date. Shows dates with email counts.

### Email List
Table of backed-up emails:
| Date | From | Subject | Size | Folder | Actions |
|------|------|---------|------|--------|---------|
| Jul 1 | sender@example.com | Meeting tomorrow | 45KB | INBOX | Download |

- Sort by date (newest first, default)
- Search by subject/from (client-side for now, server-side if needed)
- Pagination: 50 per page

### Download (`/browse/{account_id}/{date}/{filename}`)
- Fetches `.eml` (or `.eml.enc`) from disk
- If encrypted, decrypts via YubiKey
- Serves with `Content-Disposition: attachment; filename="original.eml"`
- Content-Type: `message/rfc822`

## Error States

- **No accounts**: Show empty state with "Add your first account" CTA
- **No emails**: Show empty state in browse
- **Sync error**: Red status indicator + error message tooltip
- **YubiKey not found**: Warning banner on account page

## UI Components

### Status Badge
- Green dot + "Running"
- Yellow dot + "Pending"
- Red dot + "Failed"
- Gray dot + "Never"

### Confirmation Dialogs
- Delete account: "Are you sure? This will delete the account and all synced emails."
- Sync trigger: no confirmation needed (idempotent)
