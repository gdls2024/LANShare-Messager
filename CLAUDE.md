# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

LANShare is a decentralized P2P LAN instant messaging tool written in Go. It supports CLI and Web UI modes with features including real-time chat (public/private), encrypted file transfer, user blocking, emoji support, and automatic peer discovery via UDP broadcast. All web assets are embedded into the binary using Go's `embed` package.

## Build & Run

```bash
# Build all platforms (requires Go 1.21+, CGO for SQLite)
./build.sh
# Output in build/ directory

# Build for current platform only
go build -ldflags="-s -w" -o build/lanshare main.go types.go network.go discovery.go web.go filetransfer.go

# Run
./build/lanshare              # Interactive mode
./build/lanshare -name 张三    # Specify username
./build/lanshare -cli          # CLI-only mode (no web prompt)
```

Windows cross-compilation requires mingw-w64 toolchain for CGO (SQLite dependency).

## Architecture

Single Go package (`main`) with six source files, each owning a distinct responsibility:

| File | Responsibility |
|------|---------------|
| `types.go` | All struct definitions (`P2PNode`, `Peer`, `Message`, `ChatMessage`, `FileTransferStatus`, etc.) and constants |
| `main.go` | Node initialization (`NewP2PNode`), SQLite DB setup, CLI command loop (`handleCommand`), ACL logic, `main()` entry point |
| `network.go` | TCP peer connections, ECDH key exchange (Curve25519), AES-GCM encryption/decryption, message routing (`handleMessages`), reconnection with exponential backoff |
| `discovery.go` | UDP broadcast-based peer discovery on port 9999, periodic announcements every 30s |
| `web.go` | Embedded HTTP server (`//go:embed all:web emoji_gifs.json`), REST API handlers, Go template rendering for `web/index.html` |
| `filetransfer.go` | Chunked file transfer (64KB chunks, 100MB limit), encrypted transfer, progress tracking with speed/ETA |

### Network Ports

- **TCP 8888**: P2P peer communication (JSON-encoded messages over TCP)
- **UDP 9999**: Peer discovery broadcast
- **HTTP 8080** (default, configurable): Web UI

### Encryption Flow

1. Peers exchange ECDH public keys (Curve25519) during TCP handshake
2. Shared secret derived via `curve25519.ScalarMult`
3. Chat messages and file chunks encrypted with AES-256-GCM
4. Local SQLite DB (`message.db`) stores messages encrypted with a local key

### Web UI

- `web/index.html` - Go template (uses `{{.Name}}`, `{{.LocalIP}}`, `{{.LocalPort}}`)
- `web/app.js` - SPA client with polling-based message fetching
- `web/style.css` - Styles
- `web/canvasBg.js` - Background animation
- All embedded via `//go:embed all:web emoji_gifs.json` in `web.go`

### Key REST Endpoints (web.go)

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/messages` | GET | Get in-memory messages |
| `/loadhistory` | GET | Load from SQLite with pagination |
| `/send` | POST | Send text message |
| `/sendimage` | POST | Upload and broadcast image (multipart, 5MB limit) |
| `/sendfile` | POST | Upload and transfer file (multipart, 10MB limit) |
| `/sendreply` | POST | Send reply message |
| `/users` | GET | Online user list |
| `/filetransfers` | GET | File transfer status list |
| `/fileresponse` | POST | Accept/reject file transfer |
| `/acl` | GET | Blocked users list |

## Dependencies

- `golang.org/x/crypto` - Curve25519 for ECDH key exchange
- `github.com/mattn/go-sqlite3` - SQLite driver (requires CGO)
