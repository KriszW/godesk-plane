# GoDesk Plane

A self-hostable, pure-Go implementation of the **RustDesk** Rendezvous (`hbbs`), Relay (`hbbr`), and Client HTTP API server with an embedded modern Management Console.

## Features

- **Pure-Go RustDesk Rendezvous Server (`hbbs` compatible)**
  - UDP `21116` peer ID & Curve25519/Ed25519 public key registration (`RegisterPeer`, `RegisterPk`) with UUID/IP verification and rate limiting.
  - TCP `21115` NAT type detection (`TestNatRequest`) and peer online bitmask queries (`OnlineRequest`).
  - TCP `21116` & WebSocket `21118` signaling (`PunchHoleRequest`, `PunchHole`, `FetchLocalAddr`, `LocalAddr`, `PunchHoleSent`, `RequestRelay`, `RelayResponse`, `IceCandidate`) with libsodium-compatible Ed25519-signed `IdPk` responses.
  - 100% key-file compatible with official RustDesk (`id_ed25519` / `id_ed25519.pub`).
- **Pure-Go RustDesk Relay Server (`hbbr` compatible)**
  - TCP `21117` & WebSocket `21119` encrypted relay pairing (`RequestRelay`) with key validation, zero-overhead raw bidirectional TCP forwarding, TCP↔WebSocket bridging, live active relay tracking, and byte/duration accounting.
- **RustDesk Client HTTP API (`21114`)**
  - Automatic client telemetry ingestion (`/api/sysinfo`, `/api/heartbeat`) capturing hostname, OS, CPU, RAM, username, IP, and client version.
  - Real-time client audit ingestion (`/api/audit/conn`, `/api/audit/file`, `/api/audit/alarm`) with nonce deduplication.
  - Native RustDesk client sign-in & Address Book sync (`/api/login`, `/api/logout`, `/api/currentUser`, `/api/ab/*`).
- **Embedded Management Console (`http://<host>:21114`)**
  - **Address Book**: View all connected & registered RustDesk clients with real-time online/offline status, search, tag/OS/status filters, grid/table views, device metadata editing, and **single-click remote access** (`rustdesk://<id>` & relay-forced `rustdesk://<id>/r`).
  - **Analytics**: Fleet availability, direct vs. relayed connection trends (`24h` / `7d` / `30d`), relay bandwidth volume, OS/version/NAT/connection-type distributions, top active devices, and live active relay bridges.
  - **Audit Log**: Unified security and session trail (rendezvous signaling, relay traffic, client connections, file transfers, peer registrations, admin actions, security alerts) with filtering, raw JSON metadata inspector, and **CSV / JSON export**.
  - **Server & Setup**: One-click RustDesk client import config string (`rustdesk --config <string>`), public key copy, runtime toggles (`Require Key`, `Always Use Relay`), and user management.
- **Single-Container Docker Deployment**: Multi-stage Alpine Docker image backed by embedded pure-Go SQLite (`WAL` mode).
- **Observability (OpenTelemetry + Structured Logging + Prometheus)**
  - Structured `log/slog` logging (`LOG_LEVEL`, `LOG_FORMAT=text|json`) with automatic `trace_id` & `span_id` correlation.
  - Full OpenTelemetry SDK exporting **Traces**, **Metrics**, and **Logs** over OTLP (`http/protobuf` or `grpc`) via `OTEL_EXPORTER_OTLP_ENDPOINT`.
  - Built-in Prometheus `/metrics` endpoint exposing live gauges (`godesk_peers_online`, `godesk_peers_registered`, `godesk_relay_sessions_active`), counters (`godesk_relay_bytes_total`, `godesk_rendezvous_punch_holes_total`, `godesk_http_requests_total`), and latency/duration histograms.

## Quick Start with Docker

```bash
cp .env.example .env
# Edit .env to set PUBLIC_HOST and a strong ADMIN_PASSWORD
docker compose up -d --build
```

Open the Management Console at `http://<PUBLIC_HOST>:21114` and sign in with your configured `ADMIN_USER` / `ADMIN_PASSWORD` (default: `admin` / `godesk-admin`).

### Exposed Ports

| Port | Protocol | Purpose |
| ---- | -------- | ------- |
| `21114` | TCP | RustDesk Client HTTP API & GoDesk Plane Management Console |
| `21115` | TCP | NAT Type Test (`TestNatRequest`) & Online Status (`OnlineRequest`) |
| `21116` | TCP + UDP | Rendezvous ID Registration (UDP) & Connection Signaling (TCP) |
| `21117` | TCP | Encrypted Relay Server (`hbbr`) |
| `21118` | TCP | Rendezvous WebSocket |
| `21119` | TCP | Relay WebSocket |

## Configuring RustDesk Clients

1. Open **Server & Setup** in the GoDesk Plane web console.
2. Copy the **One-Click Import Config String** and paste it into RustDesk under **Settings → Network → ID/Relay Server** (or run `rustdesk --config <string>`).
3. Alternatively, enter:
   - **ID Server**: `<PUBLIC_HOST>`
   - **Relay Server**: `<PUBLIC_HOST>`
   - **API Server**: `http://<PUBLIC_HOST>:21114`
   - **Key**: contents of `/data/id_ed25519.pub` (shown in the web UI).

## Local Development & Testing

```bash
# Run Go unit and protocol integration tests
make test

# Build frontend + single Go binary
make build

# Run end-to-end verification against a real installed RustDesk client
./scripts/test-e2e-rustdesk.sh
```

## Architecture

- `cmd/godesk-plane/` — Server entrypoint wiring all listeners and SQLite store.
- `internal/protocol/` — RustDesk `BytesCodec` framing and `AddrMangle` IPv4/IPv6 codec.
- `internal/crypto/` — Ed25519 keypair persistence and libsodium-format `IdPk` signing.
- `internal/rendezvous/` — `hbbs`-compatible UDP/TCP/NAT/WebSocket rendezvous server.
- `internal/relay/` — `hbbr`-compatible TCP/WebSocket relay bridge with traffic accounting.
- `internal/store/` — Pure-Go SQLite persistence (`modernc.org/sqlite`) for peers, users, tags, sessions, analytics, and audit logs.
- `internal/api/` — Unified HTTP server implementing RustDesk Client API (`/api/*`), Management API (`/admin/api/*`), and embedded SPA serving.
- `web/` — React + TypeScript + Vite Management Console compiled and embedded into `internal/web/dist`.
