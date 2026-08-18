# OpenCode Go Setup Guide

Track OpenCode Go subscription quotas in onWatch.

OpenCode Go does not expose a public quota API. onWatch scrapes the authenticated OpenCode Go dashboard the same way other tooling does: with your workspace ID and browser `auth` cookie.

---

## Prerequisites

- An active [OpenCode Go](https://opencode.ai) subscription
- Access to the OpenCode Go dashboard in a browser
- onWatch installed ([Quick Start](../README.md#quick-start))

---

## How It Works

onWatch polls:

```text
https://opencode.ai/workspace/{workspaceId}/go
```

using your session cookie, then extracts utilization and reset countdowns for:

- **5-Hour** (rolling / session window)
- **Weekly**
- **Monthly** (when present on the dashboard)

Parsing tries SolidJS SSR hydration data first, then falls back to the newer `data-slot="usage-item"` HTML layout. Snapshots are stored locally in SQLite like every other provider.

This is separate from `OPENCODE_ENABLED`, which only feeds ChatGPT credentials from OpenCode into the **Codex** provider.

---

## 1. Find Your Workspace ID

Open https://opencode.ai and sign in, then use either method below.

### From the browser URL

Open your OpenCode Go usage page. The URL looks like:

```text
https://opencode.ai/workspace/wrk_xxxxxxxx/go
```

Copy the `wrk_...` segment. That is your `OPENCODE_GO_WORKSPACE_ID`.

### From the authenticated Go page

Copy the `auth` cookie value as described in the next section, then run:

```bash
curl -sS --compressed \
  -H 'Cookie: auth=<your-auth-cookie-value>' \
  https://opencode.ai/go |
  grep -oE 'wrk_[A-Za-z0-9]+' |
  sort -u
```

The authenticated `/go` page contains the workspace ID. The site root does not.
`https://opencode.ai/zen` can also expose it, but `/go` is preferred for an
OpenCode Go subscription.

If the command returns multiple workspace IDs, use the one whose
`https://opencode.ai/workspace/<workspace-id>/go` page shows your Go usage.

---

## 2. Copy the Auth Cookie

1. While signed in on opencode.ai, open your browser Developer Tools
2. Go to **Application** / **Storage** → **Cookies** → `https://opencode.ai`
3. Find the `auth` cookie
4. Copy its **value** (not the `auth=` name prefix)

Treat this cookie like a password. Logging out of OpenCode, rotating sessions, or clearing cookies will invalidate it.

---

## 3. Configure accounts

The recommended path is the dashboard:

1. Open **Settings -> Providers -> OpenCode Go**
2. Select **Add Account**
3. Enter a display name, Workspace ID, and Auth Cookie
4. Repeat for every workspace you want to monitor

Accounts can be edited, disabled, deleted, or given a replacement Cookie without restarting onWatch. Deleting an account immediately erases its encrypted Cookie while retaining its quota history.

For backward compatibility, one legacy account can still be bootstrapped from `~/.onwatch/.env` (or a project `.env`):

Add both values to `~/.onwatch/.env` (or your project `.env`):

```bash
OPENCODE_GO_WORKSPACE_ID=wrk_xxxxxxxx
OPENCODE_GO_AUTH_COOKIE=your_auth_cookie_value
```

Both legacy values are required. On the first compatible startup, existing dashboard `provider_settings.opencode` values take precedence over the environment. onWatch encrypts the Cookie into the account table and removes the old plaintext fields. Environment values are ignored once a configured database account exists.

For deployments that manage secrets externally, set a stable independent encryption key:

```bash
ONWATCH_CREDENTIAL_KEY=<base64-or-hex-encoded-32-byte-key>
```

If this is omitted, onWatch generates `<database>.credential-key` with restrictive file permissions. Back up this key separately from the SQLite database. Losing it makes stored Cookies unrecoverable.

---

## 4. Migration backup and rollback

Before converting legacy OpenCode telemetry tables, onWatch creates a consistent backup named:

```text
<database>.pre-opencode-multi-account-<UTC timestamp>.bak
```

Startup aborts if that backup cannot be created. The schema conversion and history ownership backfill then run in one SQLite transaction. Existing snapshots and cycles are assigned to the stable default OpenCode account.

The pre-migration backup may contain the legacy plaintext Cookie. Protect it like a password and delete it through your normal secure-retention process after the rollback window expires.

To roll back to an older binary:

1. Stop onWatch
2. Preserve the current database and credential-key sidecar
3. Replace the database with the automatic `.bak` file
4. Start the older binary

Do not open a migrated database with an older binary and expect it to reverse the schema.

## 5. Reload / Restart

Reload providers from Settings if available, or restart onWatch:

```bash
onwatch stop
onwatch
```

Or verify in the foreground:

```bash
onwatch --debug
```

You should see the OpenCode agent start when both credentials are present.

---

## 6. Verify

- Open http://localhost:9211
- Switch to the **OpenCode** tab
- Confirm 5-Hour / Weekly cards populate (Monthly appears when OpenCode returns it)
- Charts, cycle overview, and insights begin filling after a few polls
- With multiple accounts, **All accounts** shows latest status cards only; select one account for history, cycles, sessions, and insights

---

## Dashboard

The OpenCode Go tab shows:

- Quota cards with utilization, remaining countdown, and status
- Historical chart across tracked windows
- Billing-cycle / usage-sample tables
- Burn-rate insights for the active windows
- Latest-only all-account summary with authentication status

---

## Security Notes

- Never commit `.env` or paste the cookie into issue reports / logs
- Cookies are encrypted with AES-256-GCM and account-bound authenticated data
- Account APIs return only `has_auth_cookie`; they never return plaintext or ciphertext
- Scraped HTML is not written to logs
- All processing stays local on your machine

---

## Limitations & Notes

- This integration depends on undocumented dashboard HTML. OpenCode UI changes can break parsing until onWatch is updated.
- 401 responses, login redirects, and HTTP 200 login pages mark an account `needs_reauth`; 403 marks it `unauthorized`. Other failures use bounded retry backoff.
- Cookie lifetime is controlled by OpenCode. Expect to refresh the cookie after logout or session rotation.
- Workspace ID is required; onWatch does not auto-discover workspaces.

## Resource and retention guidance

OpenCode uses a fixed two-worker pool by default, a bounded queue, 10-second request timeouts, jitter, and finite backoff. With 11 accounts, the incremental steady-state memory budget is approximately 6-12 MiB; the two capped HTML responses account for at most 4 MiB of that. The full single binary should remain below the 128 MiB target under normal operation.

At a 120-second interval, 11 accounts produce about 7,920 snapshots and up to 23,760 quota-value rows per day. Depending on quota count and SQLite page/index overhead, plan for roughly 6-15 MiB per day. No data is silently deleted in this release. A 30-90 day retention window is recommended, with a database backup before manual pruning. Dashboard history requests are bounded and only single-account history is graphed.

---

## Troubleshooting

### No OpenCode tab

- Confirm both `OPENCODE_GO_WORKSPACE_ID` and `OPENCODE_GO_AUTH_COOKIE` are set
- Restart onWatch and check `--debug` logs for missing-config messages
- In Settings → Providers, confirm OpenCode Go shows as configured / polling

### Unauthorized / forbidden / empty data

1. Re-copy a fresh `auth` cookie while signed in
2. Confirm the workspace ID matches the `/go` URL
3. In Settings, edit the affected account and replace the Cookie
4. Open the Go dashboard in your browser and verify the page still loads

### Parse failed / response format changed

OpenCode likely changed the dashboard markup. File an issue with:

- Approximate time of failure
- Whether the browser dashboard still shows 5h / weekly / monthly
- **Do not** attach cookies or full HTML dumps with session data

### Docker / headless

Pass both env vars into the container. There is no local cookie auto-detection path for OpenCode Go.

```bash
OPENCODE_GO_WORKSPACE_ID=wrk_xxxxxxxx
OPENCODE_GO_AUTH_COOKIE=your_auth_cookie_value
```

---

## Related

- Main README environment variable reference
- Codex + OpenCode ChatGPT auth (`OPENCODE_ENABLED`) is documented in [CODEX_SETUP.md](CODEX_SETUP.md)
