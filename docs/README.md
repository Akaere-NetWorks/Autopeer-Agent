# autopeer-agent

`autopeer-agent` is the node-side daemon of **AutoPeer**, an automated [DN42](https://dn42.dev) peering service. It runs on every physical DN42 node and manages the WireGuard tunnels and BIRD BGP configurations for that node's peers. It dials back to [`autopeer-center`](../README.md) over an encrypted WebSocket, applies `peer.add` / `peer.remove` commands in real time by writing config and reloading WireGuard and BIRD, and reports periodic heartbeats (WireGuard byte counters, RTT, BGP protocol state) back to the center.

The compiled binary is `autopeer-agent`; the Go module is `github.com/akaere/autopeer-agent`.

## Data path

```
WebSocket (center) ─► handler ─► wg.Manager   ─► /etc/wireguard/<iface>.conf + wg-quick
                              └► bird.Manager ─► /etc/bird/dn42/peer/dn42_<ASN>.conf + birdc configure

metrics.Collector (ticker) ─► heartbeat ─► center   (WG stats + RTT + BGP state + node metrics)
```

The center pushes commands down the encrypted WebSocket; the handler dispatches each message to the WireGuard and BIRD managers, which write config and reload the respective daemons. Independently, a metrics ticker fires on `heartbeat_interval` and sends heartbeat messages back up the same connection.

## Documentation

| Document | Covers |
|---|---|
| [getting-started.md](./getting-started.md) | Install the agent and bring up its first connection to the center |
| [configuration.md](./configuration.md) | Every config field, its default, and what it controls |
| [architecture.md](./architecture.md) | Internal packages, startup flow, and the WireGuard/BIRD managers |
| [protocol.md](./protocol.md) | The WebSocket message protocol, key exchange, and frame encryption |
| [operations.md](./operations.md) | systemd, OTA updates, BIRD/WireGuard prerequisites, and troubleshooting |

For a quick overview of the agent and its relationship to the rest of AutoPeer, see the top-level [../README.md](../README.md).

## License

MIT. Copyright (c) 2026 Akaere Networks.
