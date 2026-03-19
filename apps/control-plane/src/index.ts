import type { Env } from "./types";
import { handleRegister, handleDeregister } from "./registry";
import { handleNetMap } from "./netmap";
import { handleHeartbeat } from "./heartbeat";
import { handleTopologyWS, TopologyHub } from "./topology";

export { TopologyHub };

function requireAuth(req: Request, env: Env): Response | null {
  if (!env.NETWORK_KEY) return null; // no key configured — allow all (useful in dev)
  const auth = req.headers.get("Authorization");
  if (!auth || auth !== `Bearer ${env.NETWORK_KEY}`) {
    return Response.json({ error: "unauthorized" }, { status: 401 });
  }
  return null;
}

export default {
  async fetch(req: Request, env: Env): Promise<Response> {
    const authErr = requireAuth(req, env);
    if (authErr) return authErr;

    const url = new URL(req.url);
    const { pathname } = url;

    // POST /api/nodes/register
    if (req.method === "POST" && pathname === "/api/nodes/register") {
      return handleRegister(req, env);
    }

    // GET /api/netmap
    if (req.method === "GET" && pathname === "/api/netmap") {
      return handleNetMap(req, env);
    }

    // DELETE /api/nodes/:nodeId
    const nodeMatch = pathname.match(/^\/api\/nodes\/([^/]+)$/);
    if (req.method === "DELETE" && nodeMatch) {
      return handleDeregister(env, nodeMatch[1]);
    }

    // POST /api/nodes/:nodeId/heartbeat
    const heartbeatMatch = pathname.match(/^\/api\/nodes\/([^/]+)\/heartbeat$/);
    if (req.method === "POST" && heartbeatMatch) {
      return handleHeartbeat(req, env, heartbeatMatch[1]);
    }

    // GET /api/topology/ws
    if (req.method === "GET" && pathname === "/api/topology/ws") {
      return handleTopologyWS(req, env);
    }

    return new Response("not found", { status: 404 });
  },
};
