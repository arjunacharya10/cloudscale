import type { Env, NetMap, NodeRow, PeerInfo } from "./types";
import { getNode } from "./registry";

const ONLINE_THRESHOLD_MS = 2 * 60 * 1000; // 2 minutes

// Build the full network map for a given node.
// Uses 2 queries: one for all peers, one for all their endpoints.
export async function buildNetMap(env: Env, nodeId: string): Promise<NetMap> {
  const self = await getNode(env, nodeId);
  if (!self) throw new Error(`node ${nodeId} not found`);

  const [peersResult, endpointsResult] = await Promise.all([
    env.cloudscale.prepare("SELECT * FROM nodes WHERE node_id != ?")
      .bind(nodeId)
      .all<NodeRow>(),
    env.cloudscale.prepare(
      "SELECT node_id, endpoint FROM endpoints WHERE node_id != ? ORDER BY updated_at DESC"
    )
      .bind(nodeId)
      .all<{ node_id: string; endpoint: string }>(),
  ]);

  // Group endpoints by node_id
  const endpointsByNode = new Map<string, string[]>();
  for (const row of endpointsResult.results) {
    const list = endpointsByNode.get(row.node_id) ?? [];
    list.push(row.endpoint);
    endpointsByNode.set(row.node_id, list);
  }

  const now = Date.now();

  const peers: PeerInfo[] = peersResult.results.map((peer) => ({
    nodeId: peer.node_id,
    name: peer.node_name,
    addresses: [peer.mesh_ip],
    publicKey: peer.public_key,
    endpoints: endpointsByNode.get(peer.node_id) ?? [],
    allowedIPs: [`${peer.mesh_ip}/32`],
    online: now - peer.last_seen < ONLINE_THRESHOLD_MS,
  }));

  return {
    self: {
      nodeId: self.node_id,
      user: self.user_id,
      addresses: [self.mesh_ip],
      publicKey: self.public_key,
    },
    peers,
  };
}

// GET /api/netmap
// Identifies the requesting node via X-Node-ID header.
// TODO: replace with Zero Trust JWT parsing once auth is wired up.
export async function handleNetMap(req: Request, env: Env): Promise<Response> {
  const nodeId = req.headers.get("X-Node-ID");
  if (!nodeId) {
    return Response.json({ error: "X-Node-ID header required" }, { status: 401 });
  }

  try {
    const netmap = await buildNetMap(env, nodeId);
    return Response.json(netmap);
  } catch (err) {
    const msg = err instanceof Error ? err.message : "unknown error";
    if (msg.includes("not found")) {
      return Response.json({ error: msg }, { status: 404 });
    }
    return Response.json({ error: msg }, { status: 500 });
  }
}
