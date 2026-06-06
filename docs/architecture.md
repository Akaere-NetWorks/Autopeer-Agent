# Architecture

Internals reference for `autopeer-agent`. This document describes how the agent
is wired together at runtime: its startup sequence, the responsibility of each
package, how it stays connected to the center, and what happens when a peering is
applied. For the wire format and message reference see [protocol.md](protocol.md);
for config fields see [configuration.md](configuration.md); for day-to-day
operation see [operations.md](operations.md).

## Overview

`autopeer-agent` runs on a physical node and turns commands from `autopeer-center`
into real WireGuard interfaces and BIRD BGP sessions. It holds a single
long-lived, encrypted WebSocket connection to the center, applies inbound
commands in real time, and pushes node and peer telemetry back on a fixed
interval.

```
                         encrypted WebSocket
   autopeer-center  <───────────────────────────>  autopeer-agent
                                                        │
   peer.add / peer.remove / peers.sync / ...            │
                          ▼                              │
                       handler ──► wg.Manager   ──► /etc/wireguard/<iface>.conf + wg-quick up/down
                          │     └► bird.Manager  ──► /etc/bird/dn42/peer/dn42_<ASN>.conf + birdc configure
                          │
   heartbeat (WG stats + RTT + BGP state + node) ◄── metrics.Collector (ticker)
```

The data path is one-directional per leg: the center drives configuration
changes through `handler`, and `metrics.Collector` independently ships telemetry
back on its own ticker.

## Startup flow

`cmd/agent/main.go` performs the following sequence:

1. **Load config.** Parse the YAML file given by `-config` (default
   `/etc/autopeer-agent/config.yaml`).
2. **Open the store.** Open the BoltDB database at `cfg.StorePath` (default
   `/var/lib/autopeer-agent/peers.db`). The parent directory is created with
   `0700` permissions if missing. The running `Version` is written into the
   store.
3. **Load or generate the key pair.** If the store already holds an X25519 key
   pair it is loaded; otherwise a new one is generated and persisted. This key
   identifies the node to the center across reconnects.
4. **Create the managers.** Construct `wg.Manager`, `bird.Manager`, and the
   `handler.Handler` (wired to both managers, the store, the agent token, and the
   key pair).
5. **Create the WebSocket client.** Build `ws.Client` for the URL
   `<center_url>/api/v1/agent/ws?node_id=<node_id>`. A cached server public key
   (if present in the store) is loaded so reconnects can use `key.auth`. The
   client is given two callbacks:
   - an **on-key-exchange** handler that runs the ECDH handshake on every
     connect, and
   - an **on-connect** handler that calls `handler.RequestSync()` to pull the
     authoritative peer set from the center.
6. **Start the metrics collector.** `metrics.Collector` is created with
   `heartbeat_interval` and `bird_detail_interval`, then run in its own
   goroutine; it ticks on the heartbeat interval and sends `heartbeat` messages.

The main goroutine then blocks on `SIGINT` / `SIGTERM`; on signal it stops the
collector and closes the WebSocket client.

## Package responsibilities

| Package | Role |
|---|---|
| `internal/ws` | WebSocket client: exponential-backoff reconnect, 30s ping keepalive, 90s read deadline, and ChaCha20-Poly1305 frame encryption enabled after key exchange. |
| `internal/handler` | Dispatches all inbound messages; owns the key-exchange state machine; applies `peer.add` / `peer.remove`; serves status and sync requests; drives OTA self-update and rollback; runs network diagnostics. |
| `internal/wg` | Writes `/etc/wireguard/<iface>.conf` and calls `wg-quick up`/`down`; parses `wg show all dump` for byte counters and handshake info; pings the remote link-local address for RTT. |
| `internal/bird` | Writes `/etc/bird/dn42/peer/dn42_<ASN>.conf` and calls `birdc configure` to reload; parses `birdc show protocols` and `birdc show protocols all`; enables/disables protocols. |
| `internal/metrics` | Collects WG stats + RTT + BIRD states (and Go runtime metrics) for tracked peers and sends `heartbeat` messages. |
| `internal/store` | BoltDB-backed persistent state: peer map, version/update-id, the agent X25519 key pair, and the cached server public key. |
| `internal/crypto` | X25519 ECDH, ChaCha20-Poly1305 encryption, HKDF key derivation, and HMAC-based auth proof. |
| `internal/config` | Loads and validates the YAML config with sane defaults. |

## Connection and resilience

The WebSocket client (`internal/ws`) owns the connection lifecycle:

- **Connect URL.** It dials `<center_url>/api/v1/agent/ws?node_id=<node_id>` and
  sends an `X-Node-ID` header (and `X-Agent-Token` when a token is configured).
- **Reconnect.** Failed connects retry with exponential backoff, starting at 1s
  and doubling up to `reconnect_max_interval` seconds. The backoff resets to 1s
  after a successful connection.
- **Keepalive.** A 30s ticker sends WebSocket ping control frames; pong replies
  push the read deadline forward.
- **Read deadline.** The read loop uses a 90s read deadline, refreshed on every
  received frame and on every pong, so a silent connection is detected and torn
  down.
- **Encryption.** Frames are sent as plaintext JSON text frames only until the
  key exchange completes. On success the client calls `EnableEncryption`, after
  which all frames are ChaCha20-Poly1305-encrypted binary frames. Encryption
  state is reset on every reconnect, so the handshake runs again on each new
  connection.

On every (re)connect the client first runs the key-exchange handler, then — only
if it succeeds — the on-connect handler (`RequestSync`). See
[protocol.md](protocol.md) for the message-level handshake and the full message
catalog.

## Applying a peering

When the center sends `peer.add`, `handler` applies the change in a fixed order
and rolls back on failure:

1. **Resolve the interface name.** The WireGuard interface is `dn42<ASN>` (full
   ASN, no underscore) unless the payload overrides it via `wg_interface`. If the
   peer is already stored and its interface is up, the handler replies
   "already active"; if the interface exists but isn't tracked, it is torn down
   first.
2. **Bring up WireGuard.** Write `<wg_dir>/<iface>.conf` and run
   `wg-quick up <iface>`. The generated config carries the node's private key,
   listen port, our link-local address, `Table = off`, and a `# AutoPeer ASN`
   marker comment; `AllowedIPs` covers the DN42 ranges.
3. **Write the BIRD peer config.** Write
   `<bird_peer_dir>/dn42_<ASN>.conf` — a `protocol bgp dn42_<ASN> from dnpeers`
   block whose `neighbor` line references the remote link-local address and the
   WireGuard interface — then run `birdc configure check` followed by
   `birdc configure` to reload.
4. **Persist.** Record the peer in the store with its ASN, remote LLA, WG
   interface, BGP protocol name (`dn42_<ASN>`), and BIRD config filename
   (`dn42_<ASN>.conf`).

**Naming summary:** the WireGuard interface uses `dn42<ASN>` (no underscore,
overridable per-peer); the BIRD protocol name and config filename use
`dn42_<ASN>` and `dn42_<ASN>.conf` (with underscore).

**Automatic rollback.** If the BIRD step fails *after* WireGuard is already up,
the handler calls `wg.RemovePeerByIface(iface)` to tear the interface back down
before returning the error response, so a partial peering is never left behind.
If the final store write fails, both the BIRD config and the WireGuard interface
are removed.

**Teardown (`peer.remove`).** The handler resolves the ASN (from the payload, or
from the store if omitted), removes the BIRD config (by stored filename when
known, else by ASN) and reloads BIRD, brings the WireGuard interface down (by
stored interface name when known, else by ASN) and deletes its config file, then
removes the peer from the store.

> `peers.sync` performs the same reconciliation for the whole peer set at once:
> it recreates any missing WG/BIRD config, replaces the stored peer map with the
> center's desired state, and tears down peers the center no longer lists.

## Metrics and heartbeats

`metrics.Collector` runs a ticker on `heartbeat_interval`. On each tick it:

- reads WireGuard stats via `wg show all dump` (rx/tx byte counters, last
  handshake, actual endpoint);
- for each tracked peer with a known remote LLA, measures round-trip time by
  pinging the remote link-local address over the peer's interface (concurrently,
  bounded to a few in flight at a time);
- reads BIRD protocol states from `birdc show protocols`; and
- reads Go runtime metrics (heap/sys memory, goroutine count, process uptime).

Detailed BIRD data (routes imported/exported/preferred and BGP uptime) is more
expensive, so it is fetched only every `bird_detail_interval`-th tick via
`birdc show protocols all` and merged into the per-peer metrics when present.

All of this is marshaled into a single `heartbeat` message — a `peers` array of
per-peer metrics plus node metrics and the running version — and sent to the
center.

## Persistent state

The BoltDB store at `store_path` (default `/var/lib/autopeer-agent/peers.db`,
opened `0600`) holds:

| Data | Description |
|---|---|
| Peer map | One entry per tracked peer: ASN, remote LLA, WG interface, BGP protocol name, BIRD config filename. |
| Version / update id | The running version and the last-applied OTA update id, used to make updates idempotent. |
| Agent key pair | The node's persistent X25519 private/public keys. |
| Server public key | The center's public key, cached so reconnects can use `key.auth` instead of `key.init`. |

The store is the agent's source of truth for which peers it manages and which
interface/protocol/file names belong to each; it lets the agent reconcile real
system state against the center's desired state after a restart.

## OTA self-update

On `agent.update` the handler receives an update id, version, download URL, and
expected SHA256. It is idempotent: if the update id was already applied, or the
current binary's hash already matches, it acknowledges without changing
anything. Otherwise it downloads the new binary into
`/var/lib/autopeer-agent/`, verifies the SHA256, backs up the current binary,
replaces it, records the new version and update id in the store, sends an
`agent.updating` acknowledgement, and calls `RestartSelf()` — which exits the
process (`os.Exit(1)`) so the systemd unit restarts it on the new binary.

On `agent.rollback` the handler restores the backed-up binary for the previous
version and restarts the same way.

See [operations.md](operations.md) for how updates are triggered and monitored,
and [configuration.md](configuration.md) for the related config fields.
