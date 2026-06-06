# Configuration

`autopeer-agent` is configured from a single YAML file. By default it reads
`/etc/autopeer-agent/config.yaml`; pass `-config /path/to/config.yaml` to
override the path. Start from [`../config.example.yaml`](../config.example.yaml)
and fill in the values for your node.

```bash
./autopeer-agent -config /etc/autopeer-agent/config.yaml
```

If the file cannot be read, parsed, or fails validation, the agent logs the
error and exits. See [Getting Started](getting-started.md) for first-run setup
and [Operations](operations.md) for day-to-day management.

## Field reference

| Field (yaml key) | Type | Required | Default | Description |
|---|---|---|---|---|
| `center_url` | string | yes | — | Center WebSocket base URL (e.g. `wss://your-center.example.com`). The agent appends `/api/v1/agent/ws?node_id=<node_id>` to form the connection URL. |
| `agent_token` | string | no | — | Authentication token issued by the center admin panel. Sent on connect and used to authorize the node. |
| `node_id` | string | yes | — | Node UUID issued by the center admin panel. |
| `bird_peer_dir` | string | no | `/etc/bird/dn42/peer/` | Directory where generated BIRD peer config files (`dn42_<ASN>.conf`) are written. |
| `wg_dir` | string | no | `/etc/wireguard/` | Directory where generated WireGuard interface configs are written. |
| `birdc_path` | string | no | `/usr/sbin/birdc` | Path to the `birdc` binary used to reload BIRD. |
| `our_asn` | int | no | `4242420000` | This node's DN42 ASN. Used in interface and config naming. |
| `our_lla` | string | yes | — | This node's WireGuard link-local address (e.g. `fe80::1`). |
| `our_wg_private_key` | string | yes | — | This node's WireGuard private key. |
| `heartbeat_interval` | int | no | `60` | Heartbeat / metrics send interval, in seconds. |
| `bird_detail_interval` | int | no | `2` | Fetch detailed BIRD protocol state every Nth heartbeat tick. |
| `reconnect_max_interval` | int | no | `60` | Maximum backoff between WebSocket reconnect attempts, in seconds. |
| `store_path` | string | no | `/var/lib/autopeer-agent/peers.db` | Path to the BoltDB database file. |

The loader rejects the config with an error if any required field is empty:
`center_url`, `node_id`, `our_lla`, or `our_wg_private_key`. All other fields
fall back to the defaults above when omitted.

> `store_path` has no default inside the config loader — if it is left empty the
> agent fills it in at startup with `/var/lib/autopeer-agent/peers.db`. All other
> defaults are applied by the config loader before parsing the file.

## Example

```yaml
# Autopeer Agent Configuration
center_url: "wss://your-center.example.com"   # required
agent_token: "your-agent-token-here"
node_id: "your-node-uuid-here"                # required

# Paths
bird_peer_dir: "/etc/bird/dn42/peer/"
wg_dir: "/etc/wireguard/"
birdc_path: "/usr/sbin/birdc"

# Network identity
our_asn: 4242420000
our_lla: "fe80::1"                            # required
our_wg_private_key: "your-wireguard-private-key-here"  # required

# Intervals (seconds)
heartbeat_interval: 60
reconnect_max_interval: 60
bird_detail_interval: 2      # fetch BIRD details every Nth heartbeat tick

# Storage (optional; defaults to /var/lib/autopeer-agent/peers.db)
# store_path: "/var/lib/autopeer-agent/peers.db"
```

## Notes on specific fields

### `heartbeat_interval` and `bird_detail_interval`

The metrics collector ticks once every `heartbeat_interval` seconds, sending a
heartbeat with WireGuard stats and RTT on every tick. Detailed BIRD protocol
state is more expensive to gather, so it is fetched only every
`bird_detail_interval`-th tick. With the defaults (`heartbeat_interval: 60`,
`bird_detail_interval: 2`), heartbeats are sent every 60 seconds and full BIRD
details are included roughly every 120 seconds.

### `our_lla` and `our_wg_private_key`

These describe this node's own WireGuard identity:

- `our_lla` is the node's WireGuard link-local address. It is used as the local
  endpoint identity in generated tunnels and as the source for RTT pings to
  peers.
- `our_wg_private_key` is the node's WireGuard private key, used when writing
  generated WireGuard interface configs.

Both are required; the agent refuses to start without them.

### Interface naming

WireGuard interfaces are named `dn42<ASN>` (full ASN, no underscore) derived
from `our_asn`, while BIRD protocol names and config files use the
`dn42_<ASN>` form (with underscore). The interface name can be overridden on a
per-peer basis via the `wg_interface` field carried in the peer command from the
center; this is not a config-file field.

### `store_path`

The agent persists its state in a BoltDB file at `store_path`: the tracked peer
map, the current version and update id, the agent's X25519 key pair, and the
center's cached public key. The parent directory is created automatically at
startup. When `store_path` points inside `/var/lib/autopeer-agent/`, the
directory is created with private (`0700`) permissions.
