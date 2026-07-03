-- name: CreateSyncRun :one
INSERT INTO sync_runs (account_id, started_at, status)
VALUES (?1, datetime('now'), 'running')
RETURNING id, account_id, started_at, finished_at, emails_backed_up, errors, status, last_uid, created_at;

-- name: UpdateSyncRunStatus :one
UPDATE sync_runs SET
    finished_at = datetime('now'),
    emails_backed_up = ?1,
    errors = ?2,
    status = ?3,
    last_uid = ?4
WHERE id = ?5
RETURNING id, account_id, started_at, finished_at, emails_backed_up, errors, status, last_uid, created_at;

-- name: ListRecentRuns :many
SELECT id, account_id, started_at, finished_at, emails_backed_up, errors, status, last_uid, created_at
FROM sync_runs
WHERE account_id = ?1
ORDER BY started_at DESC, id DESC
LIMIT 20;
