# Installation

## Prerequisites

- A running [Actual Budget](https://actualbudget.org) instance (self-hosted)
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

### 3. Find your Actual Budget sync ID

In Actual Budget, go to **Settings > Sync > Show file ID**. Copy the value — this is your `ACTUAL_SYNC_ID`.

### 4. Create `docker-compose.yml`

```yaml
services:
  bankingsync:
    image: romanspies/bankingsync:latest
    container_name: bankingsync
    restart: unless-stopped

    ports:
      - "8443:8443"

    volumes:
      - bankingsync_data:/data

    environment:
      # Required
      ACTUAL_URL: "http://your-actual-instance:5006"
      ACTUAL_PASSWORD: "your-actual-password"
      ACTUAL_SYNC_ID: "your-sync-id"

      # Optional — defaults shown
      ACTUAL_ACCOUNT: "Revolut"         # Account name in Actual Budget
      SYNC_INTERVAL_HOURS: "6"          # Sync frequency
      WEB_ADDR: ":8443"                 # Web UI listen address

      # Optional — Enable Banking app ID (can also be set via web UI)
      # EB_APPLICATION_ID: ""

      # Optional — suppress your own name from appearing as a payee
      # ACCOUNT_HOLDER_NAME: "Jane Doe, Doe Jane"

      # Optional — email alerts when a bank session is about to expire
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

If your Actual Budget instance runs in Docker on the same host, add a shared network so the containers can communicate. See the included `docker-compose.yml` for an example.

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

The web UI guides you through three steps:

1. **Setup** — upload your `private.pem` and enter your Enable Banking Application ID
2. **Connect** — filter by country, select your bank, and complete the OAuth consent flow. If your bank returns multiple sub-accounts you will be asked to pick one.
3. **Status** — view connected accounts, trigger a manual sync, or renew/remove sessions

You can connect additional bank accounts at any time from the Connect page.

### 7. Verify

After the first sync cycle completes, check Actual Budget — your transactions should appear in the configured account. The logs will show:

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

./bankingsync
```

The binary stores all state in `/data` by default. Make sure the directory exists and is writable.

### Running tests

```bash
go test ./...
```

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
