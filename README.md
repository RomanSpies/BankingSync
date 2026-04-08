<p align="center">
  <img src="assets/gemini-svg.svg" alt="bankingsync" width="420">
</p>

<h3 align="center">Your European bank transactions, inside Actual Budget. Automatically.</h3>

<p align="center">
  <a href="https://hub.docker.com/r/romanspies/bankingsync">Docker Hub</a> &middot;
  <a href="https://github.com/RomanSpies/BankingSync">GitHub</a> &middot;
  <a href="INSTALLATION.md">Installation Guide</a>
</p>

---

bankingsync connects to your bank via PSD2 open banking and imports transactions into a self-hosted [Actual Budget](https://actualbudget.org) instance. It runs on your own machine as a single Docker container — no licence keys, no account limits, no phoning home. Your financial data goes from your bank to your server and nowhere else.

## Why bankingsync

- **Broad bank coverage** — works with any bank supported by [Enable Banking](https://enablebanking.com)'s PSD2 integration across Europe
- **Connect all your accounts** — add as many bank connections as you need, each mapped to its own Actual Budget account (e.g. Revolut → "Revolut", N26 → "N26")
- **No strings attached** — fully open source under AGPL-3.0, no licence keys, no paywalls, no usage caps
- **Your data stays yours** — transactions travel directly from Enable Banking to your machine, nothing is routed through third-party servers
- **Read-only access** — bankingsync uses PSD2 read-only consent, it cannot initiate payments or modify your bank account in any way
- **Pending-to-cleared lifecycle** — pending transactions are imported immediately and automatically promoted to cleared once they settle
- **Built-in deduplication** — transaction references are persisted so re-syncing the same window never produces duplicates
- **Multi-currency aware** — foreign currency transactions are recorded at the settled amount in your account's base currency
- **Rules run automatically** — any categorisation or payee rules you have configured in Actual Budget are applied to every new transaction on import
- **Email notifications** — get alerted on sync failures and before a bank session needs to be re-authorised, with a test email button to verify your setup
- **TLS out of the box** — a self-signed certificate is generated on first start so the web UI is always served over HTTPS
- **Full observability** — ship OpenTelemetry metrics and traces to your collector, and continuous profiling data to Grafana Pyroscope
- **Supply chain transparency** — every container image ships with a CycloneDX SBOM (Go modules + OS packages) viewable in the web UI, downloadable as JSON, and attached as a BuildKit attestation on Docker Hub
- **Minimal footprint** — single Go binary, single Docker container, SQLite for storage, zero runtime dependencies

## How it works

```
Your bank
   |  (read-only OAuth via Enable Banking)
   v
bankingsync (on your machine)
   |
   |--- Fetches transactions since last sync
   |--- Filters out already-imported transaction IDs
   |--- Writes new transactions to Actual Budget
   |--- Promotes pending transactions to cleared when they settle
   |--- Applies your Actual Budget rules to new transactions
   |--- Sends an alert email if anything goes wrong or a session is expiring
   |--- Logs the result to the sync history
   v
Your Actual Budget instance
```

On first run, bankingsync imports the last 30 days of transactions. After that, it syncs only new data on every cycle. The sync interval is configurable (default: every 6 hours).

## Quick start

> For a detailed walkthrough, see [INSTALLATION.md](INSTALLATION.md).

### 1. Set up Enable Banking

[Enable Banking](https://enablebanking.com) is the regulated open banking provider that connects bankingsync to your bank.

1. Sign up at [enablebanking.com](https://enablebanking.com)
2. Register a new application in the developer portal
3. Generate your RSA key pair — either let Enable Banking generate it for you during app registration (a `.pem` file will be saved to your Downloads folder), or create one yourself:

```bash
openssl genrsa -out private.pem 2048
openssl rsa -in private.pem -pubout -out public.pem
```

   If generating manually, upload `public.pem` to the developer portal.

4. Add `https://localhost:8443/callback` as an allowed redirect URI

### 2. Find your Actual Budget sync ID

In Actual Budget, go to **Settings > Sync > Show file ID**. Copy the value.

### 3. Start bankingsync

Create a `docker-compose.yml`. bankingsync waits for Actual Budget to be healthy before starting its first sync:

```yaml
services:
  actual:
    image: actualbudget/actual-server:latest
    restart: unless-stopped
    expose:
      - "5006"
    volumes:
      - actual_data:/data
    healthcheck:
      test: ["CMD-SHELL", "node src/scripts/health-check.js"]
      interval: 30s
      timeout: 5s
      retries: 3

  bankingsync:
    image: romanspies/bankingsync:latest
    restart: unless-stopped
    depends_on:
      actual:
        condition: service_healthy
    expose:
      - "8443"
    volumes:
      - bankingsync_data:/data
    environment:
      ACTUAL_URL: "http://actual:5006"
      ACTUAL_PASSWORD: "your-password"
      ACTUAL_SYNC_ID: "your-sync-id"

volumes:
  actual_data:
  bankingsync_data:
```

See the included [`docker-compose.yml`](docker-compose.yml) for a full example with all optional parameters (email notifications, sync interval, observability, networking, etc.).

```bash
docker compose up -d
```

### 4. Complete setup in the browser

Open **https://localhost:8443** (accept the self-signed cert warning).

If bankingsync is on a remote machine:

```bash
ssh -L 8443:[DOCKER_CONTAINER_IP]:8443 yourserver
```

The web UI walks you through four steps:

1. **Setup** — upload your `private.pem` and enter your Enable Banking Application ID
2. **Connect** — pick your country and bank, complete the OAuth flow
3. **Pick Account** — choose which bank sub-account to sync (showing IBAN, owner, and currency when available), which Actual Budget account to import into, and from which date to start importing
4. **Status** — see your connected accounts, sync history, and watch the first sync run

That's it. bankingsync syncs automatically from here on. Connect additional banks any time from the Connect page — each one maps to a different Actual Budget account.

## Configuration

All configuration is via environment variables. Only three are required.

| Variable | Required | Default | Description |
|---|---|---|---|
| `ACTUAL_URL` | Yes | — | URL of your Actual Budget instance |
| `ACTUAL_PASSWORD` | Yes | — | Actual Budget server password |
| `ACTUAL_SYNC_ID` | Yes | — | Budget file sync ID |
| `ACTUAL_ACCOUNT` | No | `Revolut` | Default Actual Budget account name (used as the pre-filled value when connecting a new bank; each bank can be mapped to a different account via the web UI) |
| `EB_APPLICATION_ID` | No | — | Enable Banking application ID (locks the field in the UI if set) |
| `SYNC_INTERVAL_HOURS` | No | `6` | How often to sync |
| `ACTUAL_INSECURE_TLS` | No | `false` | Skip TLS certificate verification when connecting to Actual Budget (useful for self-signed certs) |
| `ACCOUNT_HOLDER_NAME` | No | — | Your name(s) as they appear on transactions, comma-separated. Suppresses self-transfers from appearing as payees. |
| `WEB_ADDR` | No | `:8443` | Web UI listen address |
| `NOTIFY_EMAIL` | No | — | Email for sync failure alerts and session expiry warnings |
| `SMTP_HOST` | No | `smtp.gmail.com` | SMTP server |
| `SMTP_PORT` | No | `587` | SMTP port |
| `SMTP_USER` | No | — | SMTP username |
| `SMTP_PASS` | No | — | SMTP password |
| `OTLP_ENDPOINT` | No | — | OTLP gRPC endpoint (e.g. `collector:4317`) for metrics and traces |
| `PYROSCOPE_SERVER_ADDRESS` | No | — | Grafana Pyroscope URL for continuous profiling |
| `PYROSCOPE_BASIC_AUTH_USER` | No | — | Pyroscope basic auth username |
| `PYROSCOPE_BASIC_AUTH_PASSWORD` | No | — | Pyroscope basic auth password |

## Data volume

All state lives in a single Docker volume mounted at `/data`.

| Path | Description |
|---|---|
| `/data/bankingsync.db` | SQLite database — settings, bank accounts, sync log, sync state, transaction refs |
| `/data/tls.crt`, `/data/tls.key` | TLS certificate and key — auto-generated on first start |
| `/data/private.pem` | Enable Banking private key — optional alternative to uploading via the web UI |

To use your own TLS certificate, place it at `/data/tls.crt` and `/data/tls.key` before starting.

## Web UI

| Page | Path | Description |
|---|---|---|
| Setup | `/setup` | Upload PEM file and set Application ID |
| Connect | `/connect` | Browse banks by country, start OAuth, add a bank account |
| Pick Account | `/pick-account` | Choose a sub-account (shows IBAN, owner, currency), set the target Actual Budget account and sync start date |
| Status | `/status` | View accounts, sync history, trigger sync, test email, reset sync, renew or remove accounts |
| Test Email | `POST /test-email` | Send a test email to verify SMTP configuration |
| SBOM | `/sbom` | Browse the embedded CycloneDX SBOM — Go module and OS package inventory with licenses. Raw JSON download at `/sbom.json`. |
| Health | `/health` | Returns JSON with status (`ok`/`degraded`/`unhealthy`), version, connected accounts, expiring sessions, last sync info. HTTP 503 when unhealthy. |

## Session renewal

Enable Banking sessions expire after roughly 180 days. bankingsync warns you by email (if configured) when a session is within 7 days of expiry. To renew, click **Renew** on the Status page and re-authorise with your bank. No data is lost — sync state and transaction history are preserved.

## Updating

```bash
docker compose pull && docker compose up -d
```

## Building from source

```bash
git clone https://github.com/RomanSpies/BankingSync.git
cd bankingsync
go build -o bankingsync .
go test ./...
```

Requires Go 1.25+. See [INSTALLATION.md](INSTALLATION.md) for details.

## Metrics

When `OTLP_ENDPOINT` is set, bankingsync exports OpenTelemetry metrics via gRPC:

### Counters

| Metric | Description |
|---|---|
| `bankingsync_sync_runs_total` | Sync cycles completed, labelled by status |
| `bankingsync_transactions_added_total` | New transactions imported |
| `bankingsync_transactions_confirmed_total` | Pending transactions promoted to booked |
| `bankingsync_transactions_skipped_total` | Transactions skipped (already imported) |
| `bankingsync_rules_applied_total` | Rule actions applied to new transactions |
| `bankingsync_commit_errors_total` | Errors committing to Actual Budget |

### Histograms

| Metric | Description |
|---|---|
| `bankingsync_sync_duration_seconds` | Wall-clock duration of a full sync cycle |
| `bankingsync_fetch_duration_seconds` | Duration of the Enable Banking fetch |

### Gauges

| Metric | Description |
|---|---|
| `bankingsync_pending_transactions` | Pending transactions awaiting confirmation |
| `bankingsync_session_expiry_days` | Days until session expires |

## Migrating from a previous version

If upgrading from a version that used `state.json`:

1. Keep your `/data` volume in place
2. Pull and restart: `docker compose pull && docker compose up -d`
3. `state.json` is automatically migrated into `bankingsync.db` and renamed to `state.json.migrated`
4. If `/data/private.pem` exists, it is detected automatically
5. Re-authorise your bank from the Connect page (sync state and transaction refs are preserved — no duplicates)

## License

GNU Affero General Public License v3.0 — see [LICENSE](LICENSE).

See [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) for dependency licenses.
