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

# Production mode (real Lightning payments, see "Lightning Payments with NWC")
export NWC_URI="nostr+walletconnect://..."
export NWC_ALLOW_SPEND_CAPABLE=true
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
| `NWC_URI` | No | Nostr Wallet Connect string for Lightning payments. Uses mock client if not set. |
| `NWC_ALLOW_SPEND_CAPABLE` | If the connection can spend | Set to `true` to start with a connection that can spend funds (required for coinos) |
| `B2_KEY_ID` | No | Backblaze B2 key ID (enables cloud storage) |
| `B2_APP_KEY` | No | Backblaze B2 application key |
| `B2_BUCKET` | No | Backblaze B2 bucket name |
| `B2_PREFIX` | No | Optional folder prefix for B2 objects |
| `B2_PUBLIC_URL` | No | Public URL for direct B2 downloads |

## Lightning Payments with NWC

SatoshiSend receives payments through Nostr Wallet Connect (NWC, [NIP-47](https://github.com/nostr-protocol/nips/blob/master/47.md)). The server creates invoices in your wallet and gets a notification when a payer pays. It also scans the wallet every 30 seconds to find payments whose notification was lost.

The server refuses to start with a connection that can spend funds, unless you set `NWC_ALLOW_SPEND_CAPABLE=true`. Use a receive-only connection if your wallet can make one.

### 1. Create a coinos Connection

Every coinos connection can spend funds. Give the connection a spend budget of 1 sat. Then a leaked connection string can spend at most 1 sat.

1. Log in to [coinos](https://coinos.io).
2. Open the Nostr settings at `https://coinos.io/settings/nostr`.
3. Create a new NWC connection with the name `satoshisend`.
4. Set the spending budget to `1` sat.
5. Set the budget renewal to "Never".
6. Set notifications to on.
7. Copy the `nostr+walletconnect://` string.

On coinos, a budget of `0` or an empty budget means "no limit". Do not use `0`.

If notifications are off, the server still finds payments through the wallet scan. The payer then waits up to 30 seconds for the confirmation.

### 2. Run with Real Payments

```bash
export NWC_URI="nostr+walletconnect://..."
export NWC_ALLOW_SPEND_CAPABLE=true   # only for a connection that can spend
go run ./cmd/server
```

At startup, the server asks the wallet what the connection can do. It stops with an error if the wallet cannot create and list invoices.

### Security Notes

- The NWC string is a secret. Never commit it to version control.
- If the string leaks, delete the connection in your wallet and create a new one.
- Keep the spend budget small on a connection that can spend.

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
├── payments/        # Lightning payments (NWC + mock)
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
Environment=NWC_URI=nostr+walletconnect://your-connection
Environment=NWC_ALLOW_SPEND_CAPABLE=true
Environment=B2_BUCKET=your-bucket
Restart=always

[Install]
WantedBy=multi-user.target
```

systemd reads `%` in `Environment=` as a specifier. Write each `%` in the NWC string as `%%`, for example `relay=wss%%3A%%2F%%2Frelay.coinos.io`.

### Viewing Logs

```bash
# All logs
journalctl -u satoshisend

# Follow in real-time
journalctl -u satoshisend -f

# Filter by component
journalctl -u satoshisend | grep '\[nwc\]'

# Last hour only
journalctl -u satoshisend --since "1 hour ago"
```

Log prefixes: `[internal]` `[http]` `[b2]` `[nwc]`

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
