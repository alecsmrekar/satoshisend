# SatoshiSend

**Zero-knowledge file sharing with Bitcoin Lightning payments.**

<p align="center">
  <a href="https://satoshisend.xyz">https://satoshisend.xyz</a>
</p>

---

## Features

- **End-to-end encryption** — Files are encrypted in your browser using AES-256-GCM before upload. The server never sees your data.
- **Zero-knowledge architecture** — Decryption keys stay in the URL fragment (`#key`), which is never sent to the server.
- **Bitcoin Lightning payments** — Pay-per-file hosting with instant Lightning Network payments.
- **No accounts required** — Upload, pay, share. That's it.
- **Self-hostable** — Run your own instance with local storage or Backblaze B2.

## How It Works

```
┌──────────────────────────────────────────────────────────────────┐
│                         YOUR BROWSER                             │
├──────────────────────────────────────────────────────────────────┤
│  1. Select file                                                  │
│  2. Generate random AES-256 key                                  │
│  3. Encrypt file client-side                                     │
│  4. Upload encrypted blob ────────────────► Server stores blob   │
│  5. Pay Lightning invoice ────────────────► Server confirms      │
│  6. Share link: satoshisend.xyz/file/abc#key                    │
│                                        ▲                         │
│                                        │                         │
│                            Key never leaves URL fragment         │
└──────────────────────────────────────────────────────────────────┘
```

Recipients decrypt entirely in their browser — the server only ever handles encrypted data.

## Quick Start

```bash
# Clone the repository
git clone https://github.com/alecsmrekar/satoshisend.git
cd satoshisend

# Development mode (mock payments auto-settle after 20 seconds)
./run.sh

# Production mode (real Lightning payments)
export ALBY_TOKEN="your-access-token"
go run ./cmd/server
```

Open [http://localhost:8080](http://localhost:8080) in your browser.

## Configuration

### Command Line Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-addr` | `:8080` | HTTP listen address |
| `-db` | `satoshisend.db` | SQLite database path |
| `-storage` | `./uploads` | Local file storage directory |
| `-dev` | `false` | Development mode (disables CORS restrictions and rate limiting) |
| `-cors-origins` | `https://satoshisend.xyz` | Comma-separated allowed CORS origins |
| `-stats` | `false` | Show database statistics and exit |

### Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `ALBY_TOKEN` | No | Alby Wallet API token for Lightning payments. Uses mock client if not set. |
| `ALBY_WEBHOOK_SECRET` | If using Alby | SVIX webhook secret from your Alby webhook endpoint (see setup below) |
| `B2_KEY_ID` | No | Backblaze B2 key ID (enables cloud storage) |
| `B2_APP_KEY` | No | Backblaze B2 application key |
| `B2_BUCKET` | No | Backblaze B2 bucket name |
| `B2_PREFIX` | No | Optional folder prefix for B2 objects |
| `B2_PUBLIC_URL` | No | Public URL for direct B2 downloads |

## Lightning Payments with Alby

SatoshiSend uses [Alby](https://getalby.com) for Lightning Network payments.

### 1. Get an API Token

1. Log in at [getalby.com](https://getalby.com)
2. Go to [Developer Portal → Access Tokens](https://getalby.com/developer/access_tokens/new)
3. Create a token with these scopes:
   - `invoices:create` — Create payment invoices
   - `invoices:read` — Check payment status
4. Set an expiry date and click **Create**
5. Save the token as `ALBY_TOKEN`

### 2. Register a Webhook (one-time setup)

Register a webhook endpoint with Alby to receive payment notifications.

**Option A: Via the Alby Dashboard**

1. Go to [Developer Portal → Webhook Endpoints](https://getalby.com/developer/webhook_endpoints)
2. Click **Add Webhook Endpoint**
3. Set URL to `https://your-domain.com/api/webhook/alby`
4. Select event type `invoice.incoming.settled`
5. Copy the **Endpoint Secret** and save it as `ALBY_WEBHOOK_SECRET`

**Option B: Via the API**

```bash
curl -X POST https://api.getalby.com/webhook_endpoints \
  -H "Authorization: Bearer $ALBY_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "url": "https://your-domain.com/api/webhook/alby",
    "filter_types": ["invoice.incoming.settled"],
    "description": "SatoshiSend payments"
  }'
```

The response contains `endpoint_secret` — save this as `ALBY_WEBHOOK_SECRET`.

### 3. Run with Real Payments

```bash
export ALBY_TOKEN="your-access-token"
export ALBY_WEBHOOK_SECRET="whsec_..."
go run ./cmd/server
```

### Managing Webhooks

```bash
# List all webhook endpoints
curl -H "Authorization: Bearer $ALBY_TOKEN" \
  https://api.getalby.com/webhook_endpoints

# Delete a webhook endpoint
curl -X DELETE -H "Authorization: Bearer $ALBY_TOKEN" \
  https://api.getalby.com/webhook_endpoints/<endpoint-id>
```

### Security Notes

- Never commit tokens or secrets to version control
- Use minimal required scopes
- Set appropriate token expiry dates
- Treat tokens and webhook secrets like passwords

## Cloud Storage with Backblaze B2

For production, use Backblaze B2 instead of local filesystem storage.

### Setup

1. Create a B2 bucket in [Backblaze Console](https://secure.backblaze.com/b2_buckets.htm)
   - Region: **US East**
   - Access: **Private**
2. Create an application key with read/write access
3. Configure environment:

```bash
export B2_KEY_ID="your-key-id"
export B2_APP_KEY="your-application-key"
export B2_BUCKET="your-bucket-name"
export B2_PREFIX="uploads"  # optional

go run ./cmd/server
```

## Development

```bash
# Build
go build ./...

# Run tests
go test ./...

# Run with hot reload (requires air)
air
```

### Project Structure

```
cmd/server/          # Application entrypoint
internal/
├── api/             # HTTP handlers and middleware
├── files/           # File storage (filesystem + B2)
├── payments/        # Lightning payments (Alby + mock)
├── store/           # SQLite metadata storage
└── logging/         # Structured logging
web/
├── js/crypto/       # Client-side encryption (AES-256-GCM)
└── ...              # Frontend assets
```

## Deployment

### Systemd Service

```ini
[Unit]
Description=SatoshiSend
After=network.target

[Service]
Type=simple
User=satoshisend
WorkingDirectory=/opt/satoshisend
ExecStart=/opt/satoshisend/server
Environment=ALBY_TOKEN=your-token
Environment=ALBY_WEBHOOK_SECRET=whsec_your-secret
Environment=B2_BUCKET=your-bucket
Restart=always

[Install]
WantedBy=multi-user.target
```

### Viewing Logs

```bash
# All logs
journalctl -u satoshisend

# Follow in real-time
journalctl -u satoshisend -f

# Filter by component
journalctl -u satoshisend | grep '\[alby\]'

# Last hour only
journalctl -u satoshisend --since "1 hour ago"
```

Log prefixes: `[internal]` `[http]` `[b2]` `[alby]`

## Pricing Model

- **1 sat per MB** (minimum 100 sats)
- **7-day hosting** per payment
- Unpaid files are deleted after 1 hour

## License

MIT

---

<p align="center">
  <sub>Built with Go and Lightning</sub>
</p>
