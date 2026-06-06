# autopeer-agent

Node agent for the AutoPeer automated DN42 peering service.

Licensed under the MIT License. Copyright (c) 2026 Akaere Networks.

## Overview

`autopeer-agent` (`github.com/akaere/autopeer-agent`) runs on each physical node and manages WireGuard tunnels and BIRD BGP configurations for DN42 peers. It connects to `autopeer-center` over an encrypted WebSocket connection and applies `peer.add` / `peer.remove` commands in real time.

## Architecture

```
WebSocket (center) → handler → wg.Manager    → /etc/wireguard/<iface>.conf + wg-quick
                              → bird.Manager  → /etc/bird/dn42/peer/dn42_<ASN>.conf + birdc configure
metrics.Collector (ticker)    → heartbeat messages → center (WG stats + RTT + BGP state + node metrics)
```

### Package responsibilities

| Package | Role |
|---|---|
| `internal/ws` | WebSocket client with exponential-backoff reconnect, 30s ping keepalive, 90s read deadline, ChaCha20-Poly1305 frame encryption |
| `internal/handler` | Message dispatcher; key-exchange state machine; OTA update/rollback; network diagnostics (ping/trace) |
| `internal/wg` | Writes WireGuard configs and calls `wg-quick`; parses `wg show` stats; pings remote LLA for RTT |
| `internal/bird` | Writes BIRD peer configs and calls `birdc configure`; parses protocol state |
| `internal/metrics` | Collects WG stats + RTT + BIRD states + Go runtime metrics; sends `heartbeat` messages |
| `internal/store` | BoltDB persistent store: peer map, version/update-id, agent key pair, server public key |
| `internal/crypto` | X25519 ECDH + ChaCha20-Poly1305 + HKDF + HMAC auth proof |
| `internal/config` | YAML config loading with sane defaults |

### Interface naming

- WireGuard interface: `dn42<ASN>` (full ASN, no underscore) — overridable per-peer via `wg_interface`
- BIRD protocol and config file: `dn42_<ASN>` / `dn42_<ASN>.conf` (with underscore)

### Security

On each new connection the agent performs an ECDH X25519 key exchange with the center. After the handshake, all WebSocket frames are encrypted with ChaCha20-Poly1305. The server public key is cached in BoltDB so reconnects use `key.auth` instead of `key.init`.

### Message protocol

Inbound: `peer.add`, `peer.remove`, `status.request`, `peers.sync`, `peers.import`, `bird.details`, `agent.update`, `agent.resume`, `agent.rollback`, `network.ping`, `network.trace`, `key.init_ack`, `key.auth_ack`

Outbound: `response`, `status.response`, `heartbeat`, `peers.sync` (request), `key.init`, `key.auth`, `agent.updating`

### peer.add rollback

If BIRD config write fails after WireGuard is already up, the handler automatically calls `wg.RemovePeerByIface` before returning the error response.

### OTA self-update

`agent.update` triggers SHA256-verified download, backup of current binary, replacement, then process restart (via `syscall.Exec` with `os.Exit(1)` fallback). `agent.rollback` restores the backup.

## Runtime requirements

- Linux host with root or `CAP_NET_ADMIN`
- `wg` and `wg-quick` in `PATH`
- BIRD2 running with a `dnpeers` template and the peer config directory included
- `birdc` at the configured path (default `/usr/sbin/birdc`)
- `/var/lib/autopeer-agent/` writable (created automatically)

## Installation

### Automated (systemd)

```bash
bash install.sh
```

Builds the binary and installs a systemd service unit (`autopeer-agent.service`).

### Manual

```bash
go build -o autopeer-agent ./cmd/agent
./autopeer-agent -config /path/to/config.yaml
```

Default config path: `/etc/autopeer-agent/config.yaml`

## Configuration

Create a YAML config file (see `config.example.yaml`):

```yaml
center_url: "wss://your-center.example.com"
agent_token: "your-agent-token-here"
node_id: "your-node-uuid-here"

bird_peer_dir: "/etc/bird/dn42/peer/"
wg_dir: "/etc/wireguard/"
birdc_path: "/usr/sbin/birdc"

our_asn: 4242420000
our_lla: "fe80::1"
our_wg_private_key: "your-wireguard-private-key-here"

heartbeat_interval: 60       # seconds
reconnect_max_interval: 60   # seconds
bird_detail_interval: 2      # fetch BIRD details every Nth heartbeat tick
```

| Field | Default | Description |
|---|---|---|
| `center_url` | — | Center WebSocket URL (`wss://…`) |
| `agent_token` | — | Authentication token from center admin panel |
| `node_id` | — | Node UUID from center admin panel |
| `our_asn` | `4242420000` | This node's DN42 ASN |
| `our_lla` | — | This node's WireGuard link-local address |
| `our_wg_private_key` | — | This node's WireGuard private key |
| `bird_peer_dir` | `/etc/bird/dn42/peer/` | Directory for generated BIRD peer configs |
| `wg_dir` | `/etc/wireguard/` | Directory for generated WireGuard configs |
| `birdc_path` | `/usr/sbin/birdc` | Path to `birdc` binary |
| `heartbeat_interval` | `60` | Metrics send interval in seconds |
| `reconnect_max_interval` | `60` | Max backoff between reconnect attempts |
| `bird_detail_interval` | `2` | Fetch BIRD details every Nth heartbeat tick |
| `store_path` | `/var/lib/autopeer-agent/peers.db` | BoltDB database path (set in main.go) |

## Development

```bash
go build ./...
go test ./...
go test ./internal/wg/...   # single package
```

Always run `go build ./...` before pushing.

## License

Released under the [MIT License](./LICENSE). Copyright (c) 2026 Akaere Networks.
