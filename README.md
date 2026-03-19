# Cloudscale

A self-hostable mesh VPN, inspired by Tailscale. Nodes register with a central control plane, receive a mesh IP, and connect directly peer-to-peer over WireGuard.

## How it works

1. Each node registers with the control plane and gets a mesh IP from `100.64.0.0/10`
2. The control plane tracks endpoints (public + LAN IPs) via periodic heartbeats
3. Nodes receive live netmap updates over a WebSocket — no polling
4. WireGuard is configured automatically from the netmap

## Architecture

```
apps/
  control-plane/   TypeScript, Cloudflare Workers
  client/          Go, WireGuard daemon + CLI
```

**Control plane** runs on Cloudflare Workers with:
- **D1** — SQLite node registry
- **Durable Objects** (`TopologyHub`) — singleton WebSocket hub that fans out netmap updates to all connected nodes

**Client** packages:
- `internal/controlclient` — register, heartbeat, netmap, WebSocket
- `internal/wireguard` — interface setup, peer sync via wgctrl
- `internal/netmap` — translates control-plane netmap to WireGuard peer config
- `internal/endpoint` — discovers public IP via Cloudflare STUN + local IPs

## Prerequisites

**Control plane**
- [Cloudflare account](https://dash.cloudflare.com) (free tier works)
- Node.js + `wrangler` CLI (`npm i -g wrangler`)

**Client**
- Go 1.23+
- Linux: `wireguard-tools` (`apt install wireguard-tools`)
- macOS: `wireguard-tools` (`brew install wireguard-tools`)

> **Note:** Cloudscale uses `100.64.0.0/10`. If Tailscale is running on the same machine it will conflict — stop Tailscale before running Cloudscale.

## Deploying the control plane

```bash
cd apps/control-plane

# First time: create the D1 database schema
npx wrangler d1 execute cloudscale --file=schema.sql

# Set the shared network key (all clients must use the same key)
npx wrangler secret put NETWORK_KEY

# Deploy
npx wrangler deploy
```

For local development:
```bash
cp .dev.vars.example .dev.vars   # edit to set NETWORK_KEY
npx wrangler d1 execute cloudscale --local --file=schema.sql
npx wrangler dev --ip 0.0.0.0
```

## Building the client

```bash
cd apps/client

# Current machine
go build -o cloudscale ./cmd/cloudscale

# Cross-compile for Raspberry Pi (arm64)
GOOS=linux GOARCH=arm64 go build -o cloudscale-pi ./cmd/cloudscale

# Cross-compile for Raspberry Pi (older, 32-bit)
GOOS=linux GOARCH=arm GOARM=7 go build -o cloudscale-pi ./cmd/cloudscale
```

## Client setup

Run the interactive setup wizard on each node:

```bash
sudo cloudscale setup
```

This will prompt for your control plane URL, network key, and node name, then write `~/.config/cloudscale/config.json`. It will also offer to install cloudscale as a system service (systemd on Linux, launchd on macOS) so it starts automatically on boot.

If you prefer to write the config manually:

```json
{
  "controlURL": "https://<your-worker>.workers.dev",
  "networkKey": "<your-shared-secret>",
  "nodeName": "my-laptop",
  "userId": "alice"
}
```

All nodes on the same network share the same `controlURL` and `networkKey`. `nodeName` identifies the machine in `cloudscale status`.

## Usage

```bash
# First time: configure this node
sudo cloudscale setup

# Connect (requires root — manages WireGuard interface)
# Blocks in the foreground; use a service manager or screen/tmux for background use
sudo cloudscale up

# Show mesh status and connected peers
cloudscale status

# Gracefully disconnect and deregister
# Sends SIGTERM to the running cloudscale up process, then cleans up
sudo cloudscale down
```

`cloudscale up` will:
1. Register the node (or reuse existing registration from `~/.local/share/cloudscale/state.json`)
2. Generate a WireGuard keypair and bring up the interface
3. Discover public/LAN endpoints via Cloudflare STUN
4. Start a heartbeat loop (every 30s) and a WebSocket listener for live updates
5. Write a PID file so `cloudscale down` can find and stop it cleanly

`cloudscale down` will:
1. Send SIGTERM to the running `cloudscale up` process and wait for it to exit
2. Deregister the node from the control plane
3. Remove the WireGuard interface and route
4. Clean up any leftover socket files
5. Delete local state

> **Ctrl+C vs `cloudscale down`:** Ctrl+C in the `cloudscale up` terminal disconnects immediately but does not deregister — the node stays in the control plane until its heartbeat times out (~2 min). Use `cloudscale down` for a clean exit that frees the mesh IP immediately.

## Running as a service

`cloudscale setup` can install the service for you. To do it manually:

**Linux (systemd)**
```bash
sudo cloudscale setup   # choose yes when asked about service installation
sudo systemctl daemon-reload
sudo systemctl enable --now cloudscale
sudo journalctl -u cloudscale -f   # view logs
```

**macOS (launchd)**
```bash
sudo cloudscale setup   # choose yes when asked about service installation
sudo launchctl load /Library/LaunchDaemons/com.cloudscale.plist
tail -f /var/log/cloudscale.log   # view logs
```

## API reference

| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/nodes/register` | Register a node, allocates mesh IP |
| DELETE | `/api/nodes/:nodeId` | Deregister a node |
| GET | `/api/netmap` | Get network map for requesting node |
| POST | `/api/nodes/:nodeId/heartbeat` | Update last_seen + endpoints |
| GET | `/api/topology/ws` | WebSocket — receives live netmap pushes |

All requests require `Authorization: Bearer <NETWORK_KEY>`.

## Mesh IP pool

`100.64.0.0/10` (CGNAT range) — allocates sequentially from `100.64.0.1`. Supports up to ~4 million nodes.

## Roadmap

- [ ] DERP relay fallback (for symmetric NATs)
- [ ] Exit nodes
- [ ] ACLs
- [ ] Cloudflare Zero Trust auth (replace shared key)
