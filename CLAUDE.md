# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Test Commands

```bash
./run.sh                    # Run the server (localhost:8080)
go build ./...              # Build all packages
go test ./...               # Run all tests
go test ./internal/store    # Run tests for a specific package
```

## Project Overview

SatoshiSend is a file sharing application with Bitcoin-only payments. Files are encrypted client-side before upload, ensuring the server never has access to unencrypted content.

## Architecture

### Client-Side Encryption
- Files are encrypted in the browser using the WebCrypto API (AES-GCM) before upload
- The decryption key is placed in the URL anchor/fragment (e.g., `/file/abc123#base64key`)
- Since URL fragments are never sent to the server, the server cannot decrypt uploaded files
- The server acts only as encrypted blob storage

### Payment Model
- Uploader pays to host the file (1 sat per MB, minimum 100 sats, 7 days)
- Downloads are free once the file is paid for and hosted

### Payment Flow
- Bitcoin via Lightning Network for instant confirmations
- Backend integrates with LND via `LNDClient` interface
- Mock implementation (`MockLNDClient`) auto-settles invoices after 20 seconds for development

## Code Structure

```
internal/
├── store/      # Metadata persistence (Store interface + SQLite)
├── files/      # File handling (Storage interface + filesystem, Service)
├── payments/   # Lightning payments (LNDClient interface + mock, Service)
└── api/        # HTTP handlers (thin layer delegating to services)
```

### Testability
Each module uses interfaces for external dependencies, enabling mock implementations:
- `store.Store` - metadata persistence
- `files.Storage` - blob storage
- `payments.LNDClient` - Lightning Network operations

Dependencies are injected via constructors (e.g., `files.NewService(storage, store)`).
