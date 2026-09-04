# Installation

## Prerequisites

- A running [Actual Budget](https://actualbudget.org) **or** [Firefly III](https://firefly-iii.org) instance (self-hosted)
- An [Enable Banking](https://enablebanking.com) developer account and application
- Docker and Docker Compose (or Go 1.25+ to build from source)
- OpenSSL (for key generation)

## Docker (recommended)

### 1. Generate your Enable Banking key pair

Enable Banking uses asymmetric JWT authentication. Generate a 2048-bit RSA key pair:

```bash
openssl genrsa -out private.pem 2048
openssl rsa -in private.pem -pubout -out public.pem
```

Upload `public.pem` to the [Enable Banking developer portal](https://enablebanking.com). Keep `private.pem` — you will upload it through the web UI or mount it into the container.

### 2. Register the callback URL

In the Enable Banking developer portal, add the following as an allowed redirect URI for your application:

```
https://localhost:8443/callback
```

bankingsync serves HTTPS using a self-signed certificate generated on first start. You will need to accept the browser's certificate warning once.

If you access bankingsync through an SSH tunnel, the callback URL stays the same — the tunnel maps port 8443 to your local machine.

### 3. Point bankingsync at your budget

bankingsync writes into one budgeting backend, chosen with `BUDGET_BACKEND`.

**Actual Budget** (default). In Actual Budget, go to **Settings > Sync > Show file ID**. Copy the value — this is your `ACTUAL_SYNC_ID`. You also need the server URL and its password.

**Firefly III.** Set `BUDGET_BACKEND: "firefly"`. In Firefly III, go to **Options > Profile > OAuth > Personal Access Tokens**, click **Create new token**, give it a name and copy the token immediately — Firefly shows it only once. That value is your `FIREFLY_TOKEN`. Together with `FIREFLY_URL` it is all bankingsync needs; there is no separate password or file ID.

> **Balance access.** When you connect a bank, bankingsync asks Enable Banking for
> both transaction and balance access. Balances are what make the opening balance
> and the drift check work, and the scope is fixed at authorisation time — it
> cannot be widened later without re-authorising. If a bank refuses the
> authorisation request because of it, set `EB_REQUEST_BALANCE_ACCESS=false` and
> connect again; you then lose those two features but keep the import.

> Firefly III itself needs a database. If you do not run it yet, its own [Docker documentation](https://docs.firefly-iii.org/how-to/firefly-iii/installation/docker/) covers the `fireflyiii/core` plus database setup. bankingsync only needs to reach it over HTTP.

### 4. Create `docker-compose.yml`

```yaml
services:
  bankingsync:
    image: romanspies/bankingsync:latest
    container_name: bankingsync
    restart: unless-stopped

    ports:
      # Loopback only. The web UI has NO authentication — see the Security
      # section in README.md before changing this.
      - "127.0.0.1:8443:8443"

    volumes:
      - bankingsync_data:/data

    environment:
      # Required
      ACTUAL_URL: "http://your-actual-instance:5006"
      ACTUAL_PASSWORD: "your-actual-password"
      ACTUAL_SYNC_ID: "your-sync-id"
      # ACTUAL_INSECURE_TLS: "true"     # Skip TLS cert verification (for self-signed certs)

      # --- Firefly III instead of Actual Budget ---
      # Replace the three ACTUAL_* variables above with:
      # BUDGET_BACKEND: "firefly"
      # FIREFLY_URL: "http://your-firefly-instance:8080"
      # FIREFLY_TOKEN: "your-personal-access-token"
      # FIREFLY_ACCOUNT: "Checking"     # Default asset account (each bank can override via UI)
      # FIREFLY_PENDING_TAG: "pending"  # Tag on transactions the bank has not booked yet
      # FIREFLY_APPLY_RULES: "true"     # Firefly rules on booked transactions
      # FIREFLY_INSECURE_TLS: "true"    # Skip TLS cert verification
      # Optional — defaults shown
      ACTUAL_ACCOUNT: "Revolut"         # Default Actual Budget account (each bank can override via UI)
      SYNC_INTERVAL_HOURS: "6"          # Sync frequency
      WEB_ADDR: ":8443"                 # Web UI listen address

      # Optional — Enable Banking app ID (can also be set via web UI)
      # EB_APPLICATION_ID: ""

      # Optional — suppress your own name from appearing as a payee
      # ACCOUNT_HOLDER_NAME: "Jane Doe, Doe Jane"

      # Optional — email alerts on sync failures and session expiry
      # NOTIFY_EMAIL: ""
      # SMTP_HOST: "smtp.gmail.com"
      # SMTP_PORT: "587"
      # SMTP_USER: ""
      # SMTP_PASS: ""

      # Optional — observability
      # OTLP_ENDPOINT: "your-otlp-collector:4317"
      # PYROSCOPE_SERVER_ADDRESS: ""
      # PYROSCOPE_BASIC_AUTH_USER: ""
      # PYROSCOPE_BASIC_AUTH_PASSWORD: ""

volumes:
  bankingsync_data:
```

If your budget backend runs in Docker on the same host, add a shared network so the containers can communicate. See the included `docker-compose.yml` for an example.

### 5. Start the container

```bash
docker compose up -d
```

Check the logs to confirm startup:

```bash
docker compose logs -f bankingsync
```

### 6. Complete setup in the web UI

Open **https://localhost:8443** in your browser (accept the self-signed cert warning).

If bankingsync is running on a remote machine, tunnel the port first:

```bash
ssh -L 8443:localhost:8443 yourserver
```

The web UI guides you through four steps:

1. **Setup** — upload your `private.pem` and enter your Enable Banking Application ID
2. **Connect** — filter by country, select your bank, and complete the OAuth consent flow
3. **Pick Account** — choose which bank sub-account to sync (showing IBAN, owner name, and currency when available), which budget account to import into (existing ones are offered as suggestions), and from which date to start importing (defaults to 30 days ago)
4. **Status** — view connected accounts and sync history, trigger a manual sync, test your email configuration, or renew/remove sessions

You can connect additional bank accounts at any time from the Connect page. Each bank connection maps to a different budget account.

### 7. Verify

After the first sync cycle completes, check your budget — your transactions should appear in the account you selected during setup. The sync history on the Status page shows the result, and the logs will show:

```
Done: X added, Y confirmed, Z skipped
```

## Building from source

### Requirements

- Go 1.25 or later
- OpenSSL (for key generation)

### Build and run

```bash
git clone https://github.com/RomanSpies/BankingSync.git
cd bankingsync

go build -o bankingsync .
```

Set the required environment variables and run:

```bash
export ACTUAL_URL="http://localhost:5006"
export ACTUAL_PASSWORD="your-password"
export ACTUAL_SYNC_ID="your-sync-id"

# or, for Firefly III:
# export BUDGET_BACKEND="firefly"
# export FIREFLY_URL="http://localhost:8080"
# export FIREFLY_TOKEN="your-personal-access-token"

./bankingsync
```

The binary stores all state in `/data` by default. Make sure the directory exists and is writable.

### Running tests

```bash
go test ./...
```

That covers everything that needs no server. The Firefly integration tests sit
behind a build tag and write to a real instance, so they need a disposable one —
see [Building from source](README.md#building-from-source) in the README for the
container and the three environment variables they require.

## Updating

```bash
docker compose pull
docker compose up -d
```

State is stored in the `/data` volume and persists across updates.

## Using your own TLS certificate

By default, bankingsync generates a self-signed certificate on first start. To use your own:

1. Place your certificate at `/data/tls.crt` and key at `/data/tls.key`
2. Restart the container

If the files exist, the auto-generated certificate is not created.

## Mounting `private.pem` directly

Instead of uploading the private key through the web UI, you can place it at `/data/private.pem` before starting. It will be detected automatically.
