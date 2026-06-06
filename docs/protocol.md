# WebSocket Protocol

`autopeer-agent` talks to `autopeer-center` over a single long-lived WebSocket
connection. Every message is a JSON object; after the encryption handshake
completes, each frame is sealed with ChaCha20-Poly1305. This document describes
the protocol from the agent's perspective.

The center documents the same protocol from the server side in its own
`docs/websocket-protocol.md`; message names and the handshake here are kept
consistent with it.

See also: [architecture.md](architecture.md) for where the WebSocket client sits
in the agent, [configuration.md](configuration.md) for `center_url`, and
[operations.md](operations.md) for connection troubleshooting.

---

## Transport & framing

The agent dials a single WebSocket to:

```
<center_url>/api/v1/agent/ws?node_id=<node_id>
```

On connect the agent sets two HTTP headers on the upgrade request:

- `X-Node-ID` — the configured `node_id`.
- `X-Agent-Token` — the configured `agent_token` (sent when a token is
  configured; on key-auth reconnects the token may be omitted).

### Message envelope

Every message in both directions is a single JSON object with this shape:

```jsonc
{
  "type":    "peer.add",    // string, required — message type
  "id":      "f1e2...",     // string, optional — correlation id for request/response
  "payload": { /* ... */ }, // object, optional — type-specific body
  "success": true,          // bool, optional — set on `response` messages
  "error":   "..."          // string, optional — error detail when something failed
}
```

The Go struct (`internal/ws/client.go`) is:

```go
type Message struct {
    Type    string          `json:"type"`
    ID      string          `json:"id,omitempty"`
    Payload json.RawMessage `json:"payload,omitempty"`
    Success *bool           `json:"success,omitempty"`
    Error   string          `json:"error,omitempty"`
}
```

### Framing before vs. after the handshake

- **Before encryption is enabled** — frames are plaintext WebSocket **text**
  messages carrying the JSON envelope. Only the key-exchange messages flow in
  this phase.
- **After encryption is enabled** — frames are WebSocket **binary** messages.
  The JSON is encrypted with ChaCha20-Poly1305 (XChaCha20-Poly1305, 24-byte
  random nonce prepended to the ciphertext). A binary frame that fails to
  decrypt is dropped; a text frame received in encrypted mode is rejected.

### Keepalive & deadlines

The client sends a WebSocket **ping** every 30 seconds and resets a **90-second
read deadline** on every pong and every message received. If the read deadline
elapses or a read/write error occurs, the connection is torn down and the client
reconnects with exponential backoff (starting at 1s, doubling, capped by
`reconnect_max_interval`). On every (re)connect the agent re-runs the key
exchange and then requests a `peers.sync`.

---

## Encryption handshake

Before any commands are served, the agent performs an X25519 ECDH key exchange
against the center's key pair and switches the connection to a derived
ChaCha20-Poly1305 session key. The agent holds a persistent X25519 key pair in
its BoltDB store; the center's public key, once learned, is also cached there.

Primitives live in `internal/crypto/crypto.go`:

- **X25519 ECDH** — `DeriveSharedSecret` computes the shared secret from the
  agent private key and the center public key.
- **HKDF (SHA-256)** — `DeriveEncKey` derives the 32-byte session key from the
  shared secret and a nonce, with the fixed info string `autopeer-encryption-key`.
- **ChaCha20-Poly1305** — `NewAEAD` builds an XChaCha20-Poly1305 AEAD from the
  session key; `Encrypt` / `Decrypt` seal and open frames.
- **HMAC auth proof** — `ComputeAuthProof` computes
  `HMAC-SHA256(shared_secret, "autopeer-key-auth" ‖ nonce)`.

Which message the agent sends depends on whether it already has the center's
public key cached:

- **First connection (no cached server key)** → the agent sends `key.init`.
- **Subsequent connections (server key cached in BoltDB)** → the agent sends
  `key.auth`.

### `key.init` flow (first connection)

```
agent                                         center
  │ ── key.init { pubkey } ───────────────────▶ │
  │ ◀──────── key.init_ack { pubkey, nonce } ── │
  │   (derive key, enable encryption)            │
```

1. The agent sends `key.init` with its X25519 public key (hex):
   `{ "pubkey": "<agent hex>" }`.
2. The center replies `key.init_ack` with its own public key and a nonce:
   `{ "pubkey": "<center hex>", "nonce": <bytes> }`. (On rejection,
   `key.init_ack` carries an `error`; an `error` of `reset_required` means an
   operator must clear this node's stored pubkey on the center before the agent
   can re-pair.)
3. The agent derives the shared secret via ECDH, derives the session key via
   HKDF over `(shared, nonce)`, **caches the center public key in BoltDB**, and
   calls `EnableEncryption`. All subsequent frames are encrypted.

The agent waits up to **15 seconds** for `key.init_ack`; a timeout fails the
handshake and the connection is retried.

### `key.auth` flow (reconnect, server key cached)

```
agent                                                         center
  │ ── key.auth { node_id, pubkey, nonce, proof } ─────────────▶ │
  │ ◀── key.auth_ack { pubkey, auth_nonce, center_nonce } ────── │
  │   (derive session key, enable encryption)                    │
```

1. The agent generates a fresh 32-byte `nonce`, loads the cached center pubkey,
   computes the shared secret, and computes the HMAC `proof` over the nonce.
2. It sends `key.auth` with
   `{ "node_id", "pubkey": "<agent hex>", "nonce": <bytes>, "proof": <bytes> }`.
   The HMAC proof lets the center verify the agent without re-pinning the key.
3. The center replies `key.auth_ack` with a fresh **ephemeral** public key plus
   two nonces: `{ "pubkey": "<ephemeral hex>", "auth_nonce": <bytes>,
   "center_nonce": <bytes> }`.
4. The agent derives a per-session shared secret from
   `agent_priv · ephemeral_pub`, then derives the session key via HKDF over the
   shared secret and the concatenated nonce `auth_nonce ‖ center_nonce`, and
   calls `EnableEncryption`.

The agent waits up to **15 seconds** for `key.auth_ack`.

In both flows, once `EnableEncryption` is called the connection switches to
encrypted binary frames and the agent proceeds to request a `peers.sync`.

---

## Inbound messages (center → agent)

All inbound types are dispatched by `HandleMessage` in
`internal/handler/handler.go`. Unknown types are logged and ignored. Most
commands carry a correlation `id` and are answered with a `response` (or, for
`status.request`, a `status.response`) echoing that `id`.

| Type | Purpose | Notable payload fields |
|---|---|---|
| `peer.add` | Install a WireGuard tunnel + BIRD BGP config for one peer. If the BIRD write fails after WireGuard is up, WireGuard is torn down before the error response. | `peer_id`, `asn`, `remote_wg_pubkey`, `remote_endpoint`, `remote_lla`, `listen_port`, `wg_interface`, optional `mtu`, optional `wg_preshared_key` |
| `peer.remove` | Tear down a peer's WireGuard + BIRD config and forget it. ASN is resolved from the store if not supplied. | `peer_id`, `asn` |
| `status.request` | Request a one-shot snapshot of WireGuard stats and BIRD protocol states. Answered with `status.response`. | (none) |
| `peers.sync` | Authoritative full peer list pushed by the center; the agent reconciles its config to match — creating missing peers and removing stale ones. Answered with a `response` carrying `applied` / `failed` peer-id lists. | `peers[]` (each: `peer_id`, `asn`, `remote_endpoint`, `remote_wg_pubkey`, `remote_lla`, `listen_port`, `wg_interface`, `bgp_proto_name`, `bird_config_filename`, optional `mtu`, optional `wg_preshared_key`) |
| `peers.import` | Scan existing WireGuard + BIRD peers on the node and return any not yet tracked, for the center to adopt. | (none); reply payload: `peers[]` of scanned peers |
| `bird.details` | Return per-protocol BIRD detail (state, route counts, uptime) for diagnostics. | (none); reply payload: `details[]` with `name`, `state`, `routes_imported`, `routes_exported`, `bgp_uptime_secs` |
| `bird.enable` | Enable a BGP protocol for a peer (`birdc enable`). | `peer_id`, `proto_name` |
| `bird.disable` | Disable a BGP protocol for a peer (`birdc disable`). | `peer_id`, `proto_name` |
| `agent.update` | OTA self-update: SHA256-verified download, back up the current binary, replace it, then restart. Skipped if the update id or hash already matches. | `update_id`, `version`, `url`, `sha256` |
| `agent.rollback` | Restore the backed-up previous binary and restart. | (none) |
| `agent.resume` | Acknowledge that a maintenance/merge window is over and resume normal operation. | (none) |
| `network.ping` | Looking-glass ICMP ping from the node to an allow-listed target. Answered with a `response` carrying ping stats. | `target`, optional `count` (default 4, max 20) |
| `network.trace` | Looking-glass traceroute from the node. Answered with a `response` carrying parsed hops. | `target`, optional `max_hops` (default 30, max 64) |
| `network.mtr` | Looking-glass MTR from the node (JSON report, with plain-text fallback). | `target`, optional `cycles` (default 5, max 10) |
| `network.bgp_route` | BIRD route lookup for an IP or CIDR prefix (hostnames not allowed). | `target`, optional `detailed` |
| `key.init_ack` | Center's reply in the `key.init` handshake. | `pubkey`, `nonce` (or `error`) |
| `key.auth_ack` | Center's reply in the `key.auth` handshake. | `pubkey` (ephemeral), `auth_nonce`, `center_nonce` |

> Looking-glass targets (`network.*`) are validated against an allow-list:
> public unicast and DN42 ranges are permitted, while loopback, link-local,
> multicast, unspecified, broadcast, and non-DN42 private/ULA addresses are
> rejected. Hostnames are resolved to a pinned IP (3s timeout) before the command
> runs; `network.bgp_route` accepts a literal IP/prefix only.

---

## Outbound messages (agent → center)

| Type | Purpose | Notable fields |
|---|---|---|
| `response` | Generic acknowledgement of a command, correlated by the echoed `id`. On success, `success` is `true` and the `payload` may carry result data; on failure, `error` holds the reason. | `id`, `success`, `error`, optional `payload` |
| `status.response` | Reply to `status.request`. | `id`, `payload` with `wg_stats` and `bird_states` |
| `heartbeat` | Periodic telemetry pushed on the `heartbeat_interval` ticker (no `id`). | `node_id`, `timestamp`, `version`, node runtime metrics (memory, goroutine count, uptime), and a `peers[]` array with per-peer WireGuard byte counters, last handshake, RTT, BGP state, and route counts |
| `peers.sync` | **Request** to the center for the authoritative peer list (sent with no payload right after the handshake). The center answers by pushing a `peers.sync` command (see inbound). | (none) |
| `key.init` | First-connection handshake start. | `payload.pubkey` |
| `key.auth` | Reconnect handshake start. | `payload`: `node_id`, `pubkey`, `nonce`, `proof` |
| `agent.updating` | Sent during `agent.update` to tell the center the node is updating, so it is not flagged offline during the restart. | `payload.update_id` |

> `heartbeat` payload fields are produced by `internal/metrics` and consumed by
> the center; the exact field set is documented authoritatively on the server
> side. From the agent, a heartbeat always identifies the node and version and
> includes one entry per tracked peer.

---

## Request/response & timeouts

- **Correlation by `id`.** When the center issues a command that expects a
  reply, it generates a UUID `id`. The agent echoes that same `id` on its
  `response` (or `status.response`), and the center matches the reply to the
  waiting caller. Messages with no expected reply (e.g. `heartbeat`, the
  `peers.sync` request) carry no `id`.
- **Acknowledgement.** Commands such as `peer.add`, `peer.remove`,
  `bird.enable`, `bird.disable`, `agent.resume`, and `agent.rollback` are
  acknowledged with a `response` whose `success` is `true` on completion or
  whose `error` describes the failure. Data-returning commands
  (`status.request`, `peers.sync`, `peers.import`, `bird.details`,
  `network.*`) reply with a `response`/`status.response` whose `payload` carries
  the result.
- **Command timeout.** The center wraps each outbound command in a 30-second
  deadline; an agent that does not reply within the window (or whose connection
  drops) fails the command on the center side. The agent's own handshake waits
  (15 seconds for each `*_ack`) are independent of this.
- **Send buffering.** Outbound frames are queued on a bounded channel; if the
  buffer is full, `Send` returns a `send buffer full` error rather than
  blocking.
