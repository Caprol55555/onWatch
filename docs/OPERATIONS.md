# Operations, retention, backup, and recovery

onWatch remains a single-process application backed by SQLite. The Operations
tab in Settings adds bounded maintenance without MySQL, Redis, a browser
automation process, or another resident service.

## Retention

Defaults:

| Data | Default | Range |
|---|---:|---:|
| Snapshots | 90 days | 1-3650 days |
| Completed reset cycles | 365 days | 1-3650 days |
| Alerts and notification deduplication | 90 days | 1-3650 days |
| Backups | 30 days | 1-3650 days |
| Rows per table per run | 1000 | 1-5000 |

Scheduled maintenance checks five minutes after process startup and then once
every 24 hours. Its last successful completion is persisted, so frequent
restarts do not create redundant backups. It creates and verifies an SQLite backup first. If backup
creation fails, no rows are deleted. Child quota rows are deleted before their
snapshot parents in one small transaction. Active reset cycles are retained.
The process uses a passive WAL checkpoint and never runs an automatic full
`VACUUM`, avoiding long application stalls.

OpenCode Go with 11 accounts and a 120-second interval is expected to grow by
roughly 6-15 MiB per day. Other providers and quota counts change the actual
rate. At the default retention, budget approximately 0.5-1.4 GiB plus backups;
30 days is more appropriate for small disks.

## Backups and restore

Backups are consistent `VACUUM INTO` snapshots stored under the database
directory's `backups/` folder (`/data/backups` in the documented container
layout). Files use mode `0600`. Creating, listing, downloading, deleting, and
staging a restore require dashboard authentication.

Backups contain encrypted provider credentials and SMTP configuration. They are
still sensitive operational data. Do not publish them, attach them to issues,
or store them in a public bucket. The credential key sidecar is deliberately not
included in a downloaded database backup. Disaster recovery therefore requires
protecting the existing credential key separately.

Restore workflow:

1. Select Restore in Settings.
2. onWatch checks that the name stays inside the backup directory and runs
   `PRAGMA integrity_check` against a read-only connection.
3. A mode-`0600` pending restore request is written; the live database is not
   replaced while SQLite is open.
4. Restart onWatch.
5. Before opening SQLite, onWatch verifies the selected backup again, creates a
   `pre-restore` rollback copy, and atomically replaces the database.

If the restored database is not suitable, select the generated `pre-restore`
backup and repeat the workflow.

## Collection health and connection tests

The Operations tab shows bounded latest-state rows, not full history. It
distinguishes healthy, stale, stopped, pending, authentication, and disabled
states. OpenCode accounts also expose the last attempt, last success, last
error classification, consecutive failures, and next retry.

Manual retry restarts only the selected provider runner. Provider account
managers retain their worker limits, timeouts, jitter, and finite backoff.

Connection-test APIs use one response contract:

```json
{
  "success": true,
  "connected": true,
  "authenticated": true,
  "quota_parsed": true,
  "stage": "complete"
}
```

Failure stages are `network`, `authentication`, and `quota_parse`. Credentials
are neither persisted nor returned. Provider response fixtures must be
sanitized before being committed; remove tokens, cookies, account identifiers,
email addresses, and full upstream request URLs.

## Alert lifecycle

OpenCode authentication errors use configurable consecutive failure and
recovery confirmations. Default values are two failures and two successful
polls. Dashboard alerts can be acknowledged, temporarily silenced, dismissed,
or automatically resolved. A recovery notification is sent once when an open
authentication incident is resolved.

## Managed updates

Containers must not mount the Docker socket into onWatch. With
`ONWATCH_UPDATE_REQUEST_PATH`, onWatch writes an atomic request that requires:

- a database backup;
- HTTP, SQLite, schema, and collector-start health checks;
- a 120-second health timeout;
- rollback on failure.

`GET /api/update/status` returns the running version, build time, schema
version, database readability, and running collector count. A host consumer may
write a bounded `<request-path>.result.json` result for display in Settings.
The host consumer owns image replacement and rollback because an application
inside a failed container cannot reliably roll itself back. This preserves the
no-Docker-socket security boundary.

## API summary

| Endpoint | Method | Purpose |
|---|---|---|
| `/api/maintenance` | GET | Database size, WAL, policy, and backup summaries |
| `/api/maintenance/run` | POST | Verified backup, one bounded cleanup batch, checkpoint |
| `/api/backups` | GET/POST/DELETE | List, create, or delete backups |
| `/api/backups/download` | GET | Download one verified backup |
| `/api/backups/restore` | POST | Verify and stage a restart-time restore |
| `/api/collection/health` | GET | Latest bounded provider/account health |
| `/api/collection/retry` | POST | Restart one provider runner |
| `/api/alerts/action` | POST | Acknowledge, silence, or resolve an alert |
| `/api/update/status` | GET | Post-update application health contract |
