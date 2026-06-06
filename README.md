# 🔐 Sanctum

> **End-to-end encrypted, terminal-native group chat and direct messaging.**

Sanctum is a self-hostable, real-time chat system built in Go. It pairs a lightweight WebSocket server with a beautiful [Bubble Tea](https://github.com/charmbracelet/bubbletea) TUI client, providing end-to-end encrypted messaging (E2EE) for both room-based group chats and direct messages — all from your terminal.

---

## ✨ Features

- **End-to-End Encryption** — Messages are encrypted with [NaCl box](https://pkg.go.dev/golang.org/x/crypto/nacl/box) (Curve25519 + XSalsa20 + Poly1305). The server never sees plaintext.
- **Group Rooms** — Join named rooms and broadcast encrypted messages to all members.
- **Direct Messages (DMs)** — Send private, one-to-one encrypted messages. Messages are queued and delivered when the recipient comes online.
- **Presence System** — Real-time online/away/offline indicators per user, tracked automatically by inactivity.
- **Passphrase-Protected Keys** — Private keys are encrypted at rest using `scrypt`-derived keys and NaCl `secretbox`, stored in `~/.sanctum/keys.json`.
- **Beautiful TUI** — A rich terminal interface powered by Charmbracelet's Bubble Tea framework with Lip Gloss styling.
- **WebSocket Transport** — Clients connect via persistent WebSocket connections with automatic ping/pong heartbeats.
- **Live Reload (dev)** — Hot-reloading for both server and client via [Air](https://github.com/air-verse/air).
- **Dockerizable** — Server is ready to be containerized.

---

## 📐 Architecture

```
┌─────────────────────────────────────────────┐
│                  sanctum server              │
│                                             │
│  HTTP :8080                                 │
│  ├── GET  /health                           │
│  ├── GET  /keys/{uid}        (public keys)  │
│  ├── GET  /rooms/{id}/keys   (room keys)    │
│  └── GET  /ws                (WebSocket)    │
│                                             │
│  Hub (goroutine)                            │
│  ├── register / unregister clients          │
│  ├── broadcast room messages                │
│  ├── route direct messages                  │
│  ├── queue DMs for offline users            │
│  └── broadcast presence updates             │
└──────────────────┬──────────────────────────┘
                   │  WebSocket (JSON envelopes)
          ┌────────┴────────┐
          │                 │
   ┌──────┴──────┐   ┌──────┴──────┐
   │  client A   │   │  client B   │
   │  (TUI)      │   │  (TUI)      │
   │             │   │             │
   │ NaCl E2EE   │   │ NaCl E2EE   │
   └─────────────┘   └─────────────┘
```

### Package Layout

```
sanctum/
├── cmd/
│   ├── server/         # Server binary entrypoint
│   └── client/         # Client binary entrypoint (keypair setup + TUI launch)
├── internal/
│   ├── hub/            # WebSocket hub: client lifecycle, routing, presence
│   ├── crypto/         # NaCl key generation, encryption/decryption, key storage
│   ├── tui/            # Bubble Tea TUI model, WebSocket client, config
│   ├── firebase/       # Firebase auth integration
│   ├── persistence/    # Message/state persistence
│   └── protocol/       # Protocol constants
└── pkg/
    └── types/          # Shared types: Envelope, MessageType, PresenceState
```

---

## 🔒 Security Model

All message content is encrypted **on the client** before being sent to the server. The server acts purely as a transport relay.

| Layer | Mechanism |
|---|---|
| Key Generation | NaCl `box.GenerateKey` (Curve25519) |
| DM Encryption | NaCl `box.Seal` (authenticated Diffie-Hellman) |
| Group Encryption | Per-recipient NaCl box (Strategy A) |
| Key Storage at Rest | `scrypt` key derivation → NaCl `secretbox` |
| File Permissions | `keys.json` stored at `0600` |
| Key Exchange | Public keys shared via server REST endpoint on join |

---

## 🚀 Getting Started

### Prerequisites

- [Go 1.21+](https://go.dev/dl/)
- `make`

### 1. Clone & Install Dependencies

```bash
git clone https://github.com/Dubjay/sanctum.git
cd sanctum
make deps
```

### 2. Run the Server

```bash
make run-server
# Server starts on :8080
```

### 3. Run the Client

In a new terminal:

```bash
make run-client
# You will be prompted for a username and passphrase
```

On **first launch**, the client will:
1. Ask for a username.
2. Generate a new Curve25519 keypair.
3. Prompt you to create (and confirm) a passphrase to encrypt your private key.
4. Save the encrypted keypair to `~/.sanctum/keys.json`.
5. Connect to the server and join the `general` room.

On **subsequent launches**, it will load the existing keypair and prompt for your passphrase to decrypt it.

---

## 🛠️ Development

### Build Binaries

```bash
make build          # Build both server and client → bin/
make server         # Build server only
make client         # Build client only
```

### Live Reload (Air)

```bash
make air-server     # Hot-reload server on file changes
make air-client     # Hot-reload client on file changes
make dev            # Alias for air-server
```

### Testing

```bash
make test           # Run all unit tests
```

### Linting & Formatting

```bash
make fmt            # Run gofmt
make lint           # Run golangci-lint (falls back to go vet)
```

### Docker

```bash
make docker-build   # Build Docker image: sanctum:latest
```

---

## 📡 Protocol

All messages are exchanged as JSON-encoded **Envelopes** over the WebSocket connection.

### Envelope Structure

```json
{
  "id":                 "uuid",
  "type":               "text | dm | join_room | leave_room | presence_update | ack | history_batch | error",
  "from_uid":           "sender-id",
  "from_name":          "Alice",
  "to_uid":             "recipient-id",
  "room_id":            "general",
  "payload":            "...",
  "timestamp":          1700000000000,
  "encrypted_payloads": {
    "uid-1": { "ciphertext": "base64...", "nonce": "base64..." }
  }
}
```

### Message Types

| Type | Direction | Description |
|---|---|---|
| `text` | Client → Server → Room | Broadcast a (possibly encrypted) room message |
| `dm` | Client → Server → Client | Send a direct message to a specific user |
| `join_room` | Client → Server | Join a room and register your public key |
| `leave_room` | Client → Server | Leave the current room |
| `presence_update` | Server → Clients | Notify room of a user's online/away/offline status |
| `history_batch` | Server → Client | Presence snapshot sent on join |
| `ack` | Server → Client | Acknowledgement of a room action |
| `error` | Server → Client | Delivery or server error |

### REST Endpoints

| Method | Path | Description |
|---|---|---|
| `GET` | `/health` | Health check |
| `GET` | `/keys/{uid}` | Fetch a user's public key (base64) |
| `GET` | `/rooms/{roomID}/keys` | Fetch all public keys in a room |

---

## 🗂️ Client Configuration

The client stores state in `~/.sanctum/`:

| File | Contents |
|---|---|
| `keys.json` | Curve25519 public key + scrypt/secretbox-encrypted private key |
| `config.json` | Firebase session tokens (API key, ID token, refresh token) |

Both files are created with `0600` permissions.

---

## 📦 Key Dependencies

| Dependency | Purpose |
|---|---|
| [`charmbracelet/bubbletea`](https://github.com/charmbracelet/bubbletea) | Elm-architecture TUI framework |
| [`charmbracelet/bubbles`](https://github.com/charmbracelet/bubbles) | TUI components (text input, viewport, etc.) |
| [`charmbracelet/lipgloss`](https://github.com/charmbracelet/lipgloss) | Terminal styling & layout |
| [`gorilla/websocket`](https://github.com/gorilla/websocket) | WebSocket server & client |
| [`golang.org/x/crypto`](https://pkg.go.dev/golang.org/x/crypto) | NaCl box/secretbox, scrypt |
| [`google/uuid`](https://github.com/google/uuid) | Message ID generation |
| [`air-verse/air`](https://github.com/air-verse/air) | Live reload for development |

---

## 🤝 Contributing

1. Fork the repository.
2. Create a feature branch: `git checkout -b feature/my-feature`
3. Commit your changes: `git commit -m 'feat: add my feature'`
4. Push and open a Pull Request.

Please run `make fmt lint test` before submitting.

---

## 📄 License

This project is unlicensed. All rights reserved by the author.
