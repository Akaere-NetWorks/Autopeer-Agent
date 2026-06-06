# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run

```bash
# Build binary
go build -o autopeer-agent ./cmd/agent

# Run with config
./autopeer-agent -config /path/to/config.yaml

# Install (Linux, builds + installs systemd service)
bash install.sh
```

Default config path when no flag given: `/etc/autopeer-agent/config.yaml`. See `config.example.yaml` for all fields.

## Testing

```bash
go test ./...
go test ./internal/wg/...   # single package
```

## Architecture

The agent connects to an AutoPeer center server over WebSocket and dynamically manages WireGuard tunnels and BIRD BGP configs for DN42 peers.

**Startup flow** (`cmd/agent/main.go`):
1. Load YAML config
2. Open BoltDB store at `cfg.StorePath` (default `/var/lib/autopeer-agent/peers.db`)
3. Load or generate an X25519 key pair (persisted in the store)
4. Create `wg.Manager`, `bird.Manager`, and `handler.Handler`
5. Create `ws.Client` at `<center_url>/api/v1/agent/ws?node_id=...`; on every (re)connect, run key exchange then `handler.RequestSync()`
6. `metrics.Collector` ticks on `heartbeat_interval` and sends `heartbeat` messages with WireGuard stats + RTT pings

**Package responsibilities:**

| Package | Role |
|---|---|
| `internal/ws` | WebSocket client with exponential-backoff reconnect, 30s ping keepalive, 90s read deadline; optional ChaCha20-Poly1305 frame encryption after key exchange |
| `internal/handler` | Dispatches all inbound messages; owns the key-exchange state machine; drives OTA self-update and rollback |
| `internal/wg` | Writes `/etc/wireguard/<iface>.conf` and calls `wg-quick up/down`; parses `wg show all dump` for stats; pings remote LLA for RTT |
| `internal/bird` | Writes `/etc/bird/dn42/peer/dn42_<ASN>.conf` and calls `birdc configure` to reload; parses `birdc show protocols` and `birdc show protocols all` |
| `internal/metrics` | Collects WireGuard stats + RTT + BIRD states for all tracked peers and sends `heartbeat` messages |
| `internal/store` | BoltDB-backed persistent store: peer map, version/update-id, agent X25519 key pair, server public key |
| `internal/crypto` | X25519 ECDH, ChaCha20-Poly1305 encryption, HKDF key derivation, HMAC-based auth proof |
| `internal/config` | Loads and validates YAML config with sane defaults |

**Interface naming:** `wg.InterfaceName(asn)` → `dn42<ASN>` (full ASN, no underscore). Overridable per-peer via `wg_interface` field. BIRD protocol name and config filename use `dn42_<ASN>` and `dn42_<ASN>.conf` (with underscore).

**Message protocol** (JSON over WebSocket):
- Inbound: `peer.add`, `peer.remove`, `status.request`, `peers.sync`, `peers.import`, `bird.details`, `agent.update`, `agent.resume`, `agent.rollback`, `key.init_ack`, `key.auth_ack`
- Outbound: `response` (ack), `status.response`, `heartbeat`, `peers.sync` (request to center), `key.init`, `key.auth`, `agent.updating`

**Key exchange flow:** On each new connection the agent sends `key.init` (first time) or `key.auth` (subsequent). The server replies with `key.init_ack` / `key.auth_ack`. On success, `ws.Client.EnableEncryption` is called and all subsequent frames are encrypted with ChaCha20-Poly1305. The server public key is cached in BoltDB so `key.auth` is used on reconnects.

**peer.add rollback:** If BIRD config write fails after WireGuard is already up, the handler automatically calls `wg.RemovePeerByIface` before returning the error response.

**OTA self-update:** `agent.update` triggers `DownloadAndApply` (SHA256-verified download to `/var/lib/autopeer-agent/`), backs up the current binary, replaces it, then calls `RestartSelf()` (which calls `os.Exit(1)` — systemd restarts the process). `agent.rollback` restores the backup.

## Runtime requirements

The agent requires root (or CAP_NET_ADMIN) on a Linux host with:
- `wg` and `wg-quick` in PATH
- `birdc` at configured path (default `/usr/sbin/birdc`)
- BIRD2 running with a `dnpeers` template defined and the peer config directory included
- `/var/lib/autopeer-agent/` writable (created automatically at startup)

## Documentation

In-depth docs live in [`docs/`](docs/README.md): `getting-started.md`, `configuration.md`, `architecture.md`, `protocol.md`, and `operations.md`. They are written directly from the source and are the primary reference for operators running the agent.

**Keep them in sync with the code.** When you change the config schema (`internal/config/`), the WebSocket message protocol or handshake (`internal/handler/`, `internal/ws/`, `internal/crypto/`), the interface-naming rules, the OTA update flow, or the runtime requirements, update the matching guide under `docs/` (and `config.example.yaml` for new config fields) in the same change.

## Pull Request Workflow

This repository follows a feature-branch + Pull Request workflow on GitHub:

1. **Start from a clean main:** `git checkout main && git pull`
2. **Delete previously merged local branches:** `git branch --merged main | grep -v 'main' | xargs -r git branch -d`
3. **Create a feature branch:** `git checkout -b feature/<short-description>`
4. **Make changes, commit, and push:** `git add -A && git commit -m "..." && git push -u origin feature/<short-description>`
5. **Open a Pull Request** on GitHub (e.g. `gh pr create`)
6. **After merge:** switch back to main, pull the merged changes, and clean up:
   ```bash
   git checkout main
   git pull
   git branch --merged main | grep -v 'main' | xargs -r git branch -d
   ```

**Rules:**
- Never commit directly to `main`. All changes go through a feature branch + Pull Request.
- Always run `go build ./...` before pushing.
- Sync with main before starting new work (`git checkout main && git pull`).
