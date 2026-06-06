# Operations

Running and maintaining `autopeer-agent` in production on a DN42 node. This
covers runtime prerequisites, the BIRD and WireGuard surface the agent drives,
managing the systemd service, applying updates, and a troubleshooting reference.

If you are setting up a node for the first time, start with
[getting-started.md](getting-started.md) and [configuration.md](configuration.md);
this document assumes the agent is already installed and configured.

## Runtime requirements

The agent applies network changes directly on the host, so it needs elevated
privileges and a working BIRD2 + WireGuard toolchain:

| Requirement | Detail |
|---|---|
| Privileges | Linux host running as `root` or with `CAP_NET_ADMIN` |
| WireGuard | `wg` and `wg-quick` available in `PATH` |
| BIRD control | `birdc` at the configured `birdc_path` (default `/usr/sbin/birdc`) |
| BIRD daemon | BIRD2 running, with a `dnpeers` template and the peer config directory included (see below) |
| State directory | `/var/lib/autopeer-agent/` writable — created automatically at startup |

The installed systemd unit runs the agent as `User=root`, which satisfies the
privilege requirement. If you run the binary manually, ensure the invoking user
can call `wg-quick`, `birdc`, and write to `bird_peer_dir`, `wg_dir`, and the
state directory.

## BIRD prerequisites

For each peer the agent writes a config file `dn42_<ASN>.conf` into `bird_peer_dir`
(default `/etc/bird/dn42/peer/`) and reloads BIRD by running `birdc configure`.
Two things in your `bird.conf` must be in place before the agent can apply peers:

1. **A template named `dnpeers`.** The generated peer config does not define the
   BGP session options itself — it only references a template:

   ```
   protocol bgp dn42_4242420000 from dnpeers {
       neighbor fe80::1 % 'dn424242420000' as 4242420000;
   }
   ```

   If the `dnpeers` template is missing, `birdc configure` (and the
   `birdc configure check` the agent runs before reloading) will fail and the
   peer will not come up.

2. **An `include` line** that pulls in the peer config directory, so BIRD picks
   up the files the agent writes there.

A minimal, illustrative `bird.conf` fragment:

```
# Minimal dnpeers template — adapt filters/options to your DN42 setup.
template bgp dnpeers {
    local as 4242420000;
    path metric 1;
    ipv6 {
        import filter dn42_import;
        export filter dn42_export;
        import limit 10000 action block;
    };
}

# Pick up agent-generated peer files (dn42_<ASN>.conf)
include "/etc/bird/dn42/peer/*.conf";
```

This snippet is intentionally generic. The `local as`, filters, channels, and
limits must match your own DN42 routing policy — copy the `dnpeers` template
your node already uses and only add the `include` line if it is not already
present. The agent's only requirement is that a template literally named
`dnpeers` exists and that the directory in `bird_peer_dir` is included.

When the agent removes a peer it deletes the corresponding `dn42_<ASN>.conf`
and runs `birdc configure` again to drop the protocol.

## WireGuard

For each peer the agent writes `<iface>.conf` into `wg_dir` (default
`/etc/wireguard/`) and brings the tunnel up with `wg-quick up <iface>`; on
removal it runs `wg-quick down <iface>` and deletes the file. The interface name
defaults to `dn42<ASN>` (the full ASN, no underscore) and can be overridden
per-peer by the center via the `wg_interface` field.

The generated interface config sets `Table = off`, `PersistentKeepalive = 25`,
and `AllowedIPs = 172.20.0.0/14, fd00::/8, fe80::/10`, using the node's
`our_wg_private_key` and `our_lla` from the config file. Because `wg-quick`
manages the link directly, any peer interface the agent created can also be
inspected with the standard tools:

```bash
wg show
wg show dn424242420000
ip -6 addr show dev dn424242420000
```

Do not bring agent-managed interfaces up or down by hand while the agent is
running — it owns their lifecycle and will recreate or tear them down on the
next `peers.sync` from the center.

## systemd service

The installer drops `autopeer-agent.service` into `/etc/systemd/system/` and
enables it. Key directives:

| Directive | Value | Why it matters |
|---|---|---|
| `ExecStart` | `/usr/local/bin/autopeer-agent -config /etc/autopeer-agent/config.yaml` | Binary + config path |
| `After` | `network-online.target bird.service` | Starts after the network and BIRD are up |
| `Wants` | `network-online.target` | Pulls in the network-online target |
| `Restart` | `always` | Restarts on crash and on the self-update exit |
| `RestartSec` | `5` | Waits 5s between restarts |
| `User` | `root` | Required for `wg-quick`/`birdc`/network changes |
| `LimitNOFILE` | `65535` | File-descriptor limit |
| `WantedBy` | `multi-user.target` | Enabled at boot |

`Restart=always` is also what makes the in-place update work: when the agent
replaces its own binary it exits, and systemd restarts the new binary
automatically.

Manage the service with the usual commands:

```bash
systemctl start autopeer-agent
systemctl stop autopeer-agent
systemctl restart autopeer-agent
systemctl status autopeer-agent
systemctl enable autopeer-agent     # start at boot (done by install.sh)
```

View logs through the journal:

```bash
journalctl -u autopeer-agent             # full history
journalctl -u autopeer-agent -f          # follow live
journalctl -u autopeer-agent -n 100      # last 100 lines
```

## Updating the agent

### OTA self-update (driven by the center)

The center can push a new build with an `agent.update` message containing the
download URL, target version, and expected SHA256. The handler
(`internal/handler/handler.go`, `internal/handler/update_helpers.go`) applies it
as follows:

1. **Idempotency check.** If the message's `update_id` was already applied, or
   the running binary's SHA256 already matches the requested hash, the agent
   acks and does nothing.
2. **Download.** `DownloadAndApply` fetches the binary over HTTPS only
   (non-HTTPS URLs are refused), sending the `agent_token` as an
   `X-Agent-Token` header, into `/var/lib/autopeer-agent/`. The download is
   size-capped and its SHA256 is verified against the expected hash before use;
   a mismatch aborts the update.
3. **Backup.** The current binary is copied to
   `/var/lib/autopeer-agent/backup/autopeer-agent.<current-version>`.
4. **Replace.** The verified download atomically replaces the running binary
   in place.
5. **Restart.** The agent sends `agent.updating`, persists the new version and
   `update_id`, acks the message, then calls `RestartSelf()`. That re-execs the
   binary and falls back to `os.Exit(1)` — either way the process ends and
   systemd (`Restart=always`) brings the new binary back up.

If the store has no record of the current version, the agent refuses the update
rather than risk a backup it cannot roll back to.

To revert, the center sends `agent.rollback`: the agent restores
`/var/lib/autopeer-agent/backup/autopeer-agent.<previous-version>` over the
current binary and restarts the same way. If no backup exists for the previous
version, the rollback is rejected.

An `agent.resume` message is acked and used by the center to signal that a
post-update merge is complete and normal operation can continue; it does not by
itself change the binary.

### Manual update

If you build and ship binaries yourself, update out-of-band:

```bash
# On the node (or build elsewhere and copy the binary over)
go build -o autopeer-agent ./cmd/agent
sudo install -m 0700 -o root -g root autopeer-agent /usr/local/bin/autopeer-agent
sudo systemctl restart autopeer-agent
```

Re-running `bash install.sh` performs the same build-and-install steps and
reloads systemd; it will not overwrite an existing
`/etc/autopeer-agent/config.yaml`.

## Troubleshooting

Always begin with `journalctl -u autopeer-agent -n 200`. The agent logs each
WireGuard and BIRD action, the key-exchange result, and the underlying
`wg-quick` / `birdc` output on failure.

| Symptom | Likely cause | Check |
|---|---|---|
| Agent won't connect to center | Wrong `center_url`, `agent_token`, or `node_id`; TLS/cert issue | Confirm `center_url` is a reachable `wss://` URL and that `agent_token` / `node_id` match the values issued by the center. Watch logs for reconnect/backoff messages. |
| Key exchange rejected (`reset_required`) | Center has a stale public key for this node | The node's cached key no longer matches the center's record; an admin must clear the node's public key on the center side, then let the agent re-handshake. |
| Peers don't come up — BIRD side | Missing `dnpeers` template or the peer dir is not `include`d | Run `birdc configure check`; look for template/parse errors. Verify `bird.conf` defines `dnpeers` and includes `bird_peer_dir`. See [BIRD prerequisites](#bird-prerequisites). |
| Peers don't come up — `birdc` errors | Wrong `birdc_path`, BIRD not running, or permission denied | Confirm `birdc_path` (default `/usr/sbin/birdc`) is correct and BIRD2 is running (`systemctl status bird`). The agent runs as root in the unit; manual runs need socket access. |
| Peers don't come up — WireGuard side | `wg-quick up` failing (key, endpoint, port conflict, missing tools) | Check logs for the `wg-quick up <iface>` output. Verify `wg`/`wg-quick` are in `PATH` and `our_wg_private_key` / `our_lla` are set correctly in the config. |
| Peer config not written / permission denied | Agent lacks privileges or `bird_peer_dir` / `wg_dir` not writable | Ensure the agent runs as root or with `CAP_NET_ADMIN` and that both directories and `/var/lib/autopeer-agent/` are writable. |
| No metrics / heartbeats at the center | Agent not connected, or no tracked peers | Confirm the WebSocket is up in the logs; heartbeats are sent on `heartbeat_interval`. Check `wg show` reports the expected interfaces. |
| OTA update failed | SHA256 mismatch, non-HTTPS URL, download too large, or unknown current version | The log line states the exact reason (`sha256 mismatch`, `refusing to download from non-HTTPS URL`, size limit, or `current version unknown`). Fix the source artifact/URL on the center and re-issue the update. |
| Stuck after an update | New binary crashing on start | `systemctl status autopeer-agent` and the journal will show the restart loop; issue `agent.rollback` from the center to restore the backed-up binary. |

For configuration field meanings see [configuration.md](configuration.md) and the
example [../config.example.yaml](../config.example.yaml); for the message
protocol see [protocol.md](protocol.md); for the overall component layout see
[architecture.md](architecture.md).
