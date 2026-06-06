# Getting Started

This guide walks through installing `autopeer-agent` on a fresh DN42 node and getting it connected to an `autopeer-center`. By the end the node will appear online in the center and accept `peer.add` / `peer.remove` commands.

For the full configuration reference, see [configuration.md](configuration.md). For BIRD setup and deeper troubleshooting, see [operations.md](operations.md).

## Prerequisites

On the node:

- A Linux host with **root** (or the `CAP_NET_ADMIN` capability).
- **WireGuard** installed — both `wg` and `wg-quick` must be in `PATH`.
- **BIRD2** installed and running, with:
  - a `dnpeers` template defined, and
  - the peer config directory (default `/etc/bird/dn42/peer/`) `include`d from your main BIRD config.
  - See [operations.md](operations.md) for the exact BIRD configuration the agent expects.
- `birdc` available (default path `/usr/sbin/birdc`).
- **Go 1.x** — only required if you build the agent from source.

On the center side:

- A running [`autopeer-center`](../README.md) that this node can reach over `wss://`.
- An **`agent_token`** and a **`node_id`** for this node. Both are issued by the center when the node is registered through its admin side. You will paste them into the agent config below.

## Install

### Path A — automated (recommended)

Clone the repository and run the installer as a user that can `sudo`:

```bash
git clone https://github.com/Akaere-NetWorks/Autopeer-Agent.git
cd Autopeer-Agent
bash install.sh
```

`install.sh` does the following:

1. Builds the binary: `go build -o autopeer-agent ./cmd/agent`.
2. Installs it to `/usr/local/bin/autopeer-agent` (mode `0700`, owned by root).
3. Creates `/etc/autopeer-agent` and `/var/lib/autopeer-agent` (mode `0700`, owned by root).
4. Copies `config.example.yaml` to `/etc/autopeer-agent/config.yaml` **only if that file does not already exist**.
5. Installs the systemd unit `autopeer-agent.service`, runs `systemctl daemon-reload`, and `systemctl enable autopeer-agent`.

It does not start the service — you edit the config first, then start it (see [Start](#start)).

### Path B — manual

Build the binary and place it where you like:

```bash
go build -o autopeer-agent ./cmd/agent
sudo install -m 0700 -o root -g root autopeer-agent /usr/local/bin/autopeer-agent
```

Create the config directory and seed a config from the example:

```bash
sudo install -d -m 0700 -o root -g root /etc/autopeer-agent
sudo install -m 0600 -o root -g root config.example.yaml /etc/autopeer-agent/config.yaml
```

The state directory `/var/lib/autopeer-agent/` is created automatically on first start, but you can pre-create it the same way if you prefer.

## Configure

Edit `/etc/autopeer-agent/config.yaml`. At minimum, set these fields:

```yaml
center_url: "wss://your-center.example.com"
agent_token: "your-agent-token-here"
node_id: "your-node-uuid-here"

our_asn: 4242420000
our_lla: "fe80::1"
our_wg_private_key: "your-wireguard-private-key-here"
```

| Field | What to put |
|---|---|
| `center_url` | The center WebSocket URL (`wss://…`). |
| `agent_token` | The authentication token issued for this node by the center. |
| `node_id` | The node UUID issued for this node by the center. |
| `our_asn` | This node's DN42 ASN. |
| `our_lla` | This node's WireGuard link-local address. |
| `our_wg_private_key` | This node's WireGuard private key. |

`center_url`, `node_id`, `our_lla`, and `our_wg_private_key` are required — the agent refuses to start without them. The path and interval fields (`bird_peer_dir`, `wg_dir`, `birdc_path`, `heartbeat_interval`, and others) have sane defaults; see [configuration.md](configuration.md) for the complete list.

## Start

With systemd:

```bash
sudo systemctl start autopeer-agent
sudo systemctl status autopeer-agent
```

`install.sh` already enables the service for boot. If you installed manually, enable it yourself:

```bash
sudo systemctl enable autopeer-agent
```

To run the agent in the foreground (useful for first-run debugging):

```bash
./autopeer-agent -config /etc/autopeer-agent/config.yaml
```

With no `-config` flag, the agent reads `/etc/autopeer-agent/config.yaml` by default.

## Verify the connection

Follow the service logs:

```bash
sudo journalctl -u autopeer-agent -f
```

On a successful first connection you should see the agent:

1. Load (or generate) its key pair.
2. Connect to the center over WebSocket and complete the key exchange.
3. Request a peer sync from the center.

On every (re)connect the agent performs the key exchange first, then immediately requests a peer sync so its WireGuard and BIRD state matches the center. After this, the node should show as **online** in the center.

## Troubleshooting first connection

- **Won't connect / authentication fails** — double-check `center_url`, `agent_token`, and `node_id` against the values issued by the center. The `center_url` must use the `wss://` scheme and be reachable from the node.
- **BIRD errors** — make sure BIRD2 is running, the `dnpeers` template is defined, and the peer config directory is `include`d from your main config. See [operations.md](operations.md).
- **WireGuard errors** — confirm `wg` and `wg-quick` are installed and in `PATH`.
- **Permission errors** — the agent must run as root (or with `CAP_NET_ADMIN`), and `/var/lib/autopeer-agent/` must be writable.

For deeper troubleshooting, see [operations.md](operations.md).
