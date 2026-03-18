# Cloudscale

A self-hostable Tailscale-like mesh VPN.

## Architecture

**Monorepo** with two apps:

- `apps/control-plane` — TypeScript, Cloudflare Workers (coordination layer)
- `apps/client` — Go (WireGuard, daemon, UI)

## Control Plane

Hosted on Cloudflare Workers. Uses:
- **D1** (`binding = "cloudscale"`) — SQLite node registry
- **Durable Objects** (`TopologyHub`) — singleton WebSocket hub, fans out netmap updates

### API

| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/nodes/register` | Register a node, allocates mesh IP |
| DELETE | `/api/nodes/:nodeId` | Deregister a node |
| GET | `/api/netmap` | Get network map for requesting node |
| POST | `/api/nodes/:nodeId/heartbeat` | Update last_seen + endpoints, triggers broadcast |
| GET | `/api/topology/ws` | WebSocket — receives netmap pushes on topology change |

### Auth
Parked for now. `X-Node-ID` header is used as a placeholder everywhere.
Plan: Cloudflare Zero Trust JWT (`CF-Access-JWT-Assertion` header).

### Netmap shape
```json
{
  "self": { "nodeId", "user", "addresses", "publicKey" },
  "peers": [{ "nodeId", "name", "addresses", "publicKey", "endpoints", "allowedIPs", "online" }]
}
```

### Mesh IP pool
`100.64.0.0/10` (CGNAT range, same as Tailscale). Allocated sequentially from `100.64.0.1`.

### Key decisions
- `serializeAttachment`/`deserializeAttachment` used instead of `getTags()` in TopologyHub — `getTags()` is not reliably available in `wrangler dev`
- D1 binding name is `cloudscale` (not `DB`) — matches `wrangler.toml`
- Heartbeat uses `db.batch()` to atomically update `last_seen` + replace endpoints
- `online` = `lastSeen` within 2 minutes

## Client (Go)

Not yet implemented. Packages:
- `internal/controlclient` — talks to control plane (register, netmap, heartbeat)
- `internal/wireguard` — manages local WireGuard interface
- `internal/netmap` — translates netmap into WireGuard peer config

## Local Dev

```bash
cd apps/control-plane
npx wrangler d1 execute cloudscale --local --file=schema.sql  # first time only
npx wrangler dev
```

## Phase 2 (not started)
- DERP / relay fallback
- Exit nodes
- ACLs
- Cloudflare Zero Trust auth
