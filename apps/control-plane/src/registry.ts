import type { Env, NodeRow } from "./types";

// POST /api/nodes/register
// Body: { nodeName: string, publicKey: string, userId: string }
export async function handleRegister(req: Request, env: Env): Promise<Response> {
  let body: { nodeName: string; publicKey: string; userId: string };
  try {
    body = await req.json();
  } catch {
    return Response.json({ error: "invalid json" }, { status: 400 });
  }

  if (!body.nodeName || !body.publicKey || !body.userId) {
    return Response.json({ error: "nodeName, publicKey and userId are required" }, { status: 400 });
  }

  // Reject duplicate public keys
  const existing = await env.cloudscale.prepare(
    "SELECT node_id FROM nodes WHERE public_key = ?"
  ).bind(body.publicKey).first<{ node_id: string }>();

  if (existing) {
    return Response.json({ error: "public key already registered" }, { status: 409 });
  }

  const usedIPs = await env.cloudscale.prepare(
    "SELECT mesh_ip FROM nodes"
  ).all<{ mesh_ip: string }>();

  const meshIp = allocateMeshIP(usedIPs.results.map((r) => r.mesh_ip));
  const nodeId = crypto.randomUUID();
  const now = Date.now();

  await env.cloudscale.prepare(
    "INSERT INTO nodes (node_id, node_name, user_id, public_key, mesh_ip, created_at, last_seen) VALUES (?, ?, ?, ?, ?, ?, ?)"
  ).bind(nodeId, body.nodeName, body.userId, body.publicKey, meshIp, now, now).run();

  return Response.json({ nodeId, nodeName: body.nodeName, meshIp }, { status: 201 });
}

// Allocate the next available IP from 100.64.0.0/10 (100.64.0.1 – 100.127.255.254)
export function allocateMeshIP(usedIPs: string[]): string {
  const used = new Set(usedIPs);
  const start = ipToInt("100.64.0.1");
  const end = ipToInt("100.127.255.254");

  for (let i = start; i <= end; i++) {
    const ip = intToIP(i);
    if (!used.has(ip)) return ip;
  }

  throw new Error("mesh IP pool exhausted");
}

// DELETE /api/nodes/:nodeId
// Cascades to endpoints via FK. Caller should also close any open WS sessions.
export async function handleDeregister(env: Env, nodeId: string): Promise<Response> {
  const node = await getNode(env, nodeId);
  if (!node) {
    return Response.json({ error: "node not found" }, { status: 404 });
  }

  await env.cloudscale.prepare("DELETE FROM nodes WHERE node_id = ?").bind(nodeId).run();

  return new Response(null, { status: 204 });
}

export async function getNode(env: Env, nodeId: string): Promise<NodeRow | null> {
  return env.cloudscale.prepare(
    "SELECT * FROM nodes WHERE node_id = ?"
  ).bind(nodeId).first<NodeRow>() ?? null;
}

function ipToInt(ip: string): number {
  return ip.split(".").reduce((acc, octet) => (acc << 8) | parseInt(octet, 10), 0) >>> 0;
}

function intToIP(n: number): string {
  return [(n >>> 24) & 0xff, (n >>> 16) & 0xff, (n >>> 8) & 0xff, n & 0xff].join(".");
}
