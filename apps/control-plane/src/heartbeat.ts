import type { Env } from "./types";
import { getNode } from "./registry";

// POST /api/nodes/:nodeId/heartbeat
// Body: { endpoints: string[] }
// Updates last_seen, replaces endpoint list, then signals TopologyHub to broadcast.
export async function handleHeartbeat(req: Request, env: Env, nodeId: string): Promise<Response> {
  const node = await getNode(env, nodeId);
  if (!node) {
    return Response.json({ error: "node not found" }, { status: 404 });
  }

  let body: { endpoints: string[] };
  try {
    body = await req.json();
  } catch {
    return Response.json({ error: "invalid json" }, { status: 400 });
  }

  if (!Array.isArray(body.endpoints)) {
    return Response.json({ error: "endpoints must be an array" }, { status: 400 });
  }

  const now = Date.now();

  // Batch: update last_seen + replace endpoints atomically
  await env.cloudscale.batch([
    env.cloudscale.prepare("UPDATE nodes SET last_seen = ? WHERE node_id = ?")
      .bind(now, nodeId),
    env.cloudscale.prepare("DELETE FROM endpoints WHERE node_id = ?")
      .bind(nodeId),
    ...body.endpoints.map((ep) =>
      env.cloudscale.prepare("INSERT INTO endpoints (node_id, endpoint, updated_at) VALUES (?, ?, ?)")
        .bind(nodeId, ep, now)
    ),
  ]);

  // Signal TopologyHub to push fresh netmaps to all connected nodes
  const hub = env.TOPOLOGY_HUB.get(env.TOPOLOGY_HUB.idFromName("global"));
  await hub.fetch("http://internal/broadcast", { method: "POST" });

  return Response.json({ ok: true });
}
