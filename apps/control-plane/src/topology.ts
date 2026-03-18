import type { Env } from "./types";
import { buildNetMap } from "./netmap";

// Durable Object — singleton ("global") that owns all active WebSocket sessions.
// Uses the Hibernatable WebSocket API so the DO can sleep between events.
//
// Internal routes (called by the Worker, not the client):
//   GET  /ws           — WebSocket upgrade; X-Node-ID header identifies the node
//   POST /broadcast    — triggered by heartbeat to push fresh netmaps to all sessions
export class TopologyHub {
  private state: DurableObjectState;
  private env: Env;

  constructor(state: DurableObjectState, env: Env) {
    this.state = state;
    this.env = env;
  }

  async fetch(req: Request): Promise<Response> {
    const url = new URL(req.url);

    if (req.method === "GET" && url.pathname === "/ws") {
      return this.handleUpgrade(req);
    }

    if (req.method === "POST" && url.pathname === "/broadcast") {
      await this.broadcastNetMap();
      return new Response("ok");
    }

    return new Response("not found", { status: 404 });
  }

  // Upgrade the HTTP connection to a WebSocket and register the session.
  // The node is identified via X-Node-ID (forwarded from the outer Worker).
  // TODO: replace X-Node-ID with a validated Zero Trust JWT claim once auth is wired up.
  private handleUpgrade(req: Request): Response {
    const nodeId = req.headers.get("X-Node-ID");
    if (!nodeId) {
      return new Response("X-Node-ID header required", { status: 400 });
    }

    const pair = new WebSocketPair();
    const [client, server] = Object.values(pair);

    // Tag the server-side socket with the nodeId so we can identify it after hibernation
    this.state.acceptWebSocket(server, [nodeId]);
    // Store nodeId as attachment — survives hibernation, avoids relying on getTags()
    (server as unknown as { serializeAttachment(v: unknown): void }).serializeAttachment(nodeId);

    return new Response(null, { status: 101, webSocket: client });
  }

  // Called by the Workers runtime when a hibernated client sends a message.
  // Clients don't send messages in MVP, but we handle it to avoid silent drops.
  async webSocketMessage(_ws: WebSocket, _message: string | ArrayBuffer): Promise<void> {
    // no-op for MVP
  }

  async webSocketClose(ws: WebSocket): Promise<void> {
    ws.close();
  }

  async webSocketError(ws: WebSocket): Promise<void> {
    ws.close();
  }

  // Build a fresh netmap for every connected node and push it over their WebSocket.
  private async broadcastNetMap(): Promise<void> {
    const sockets = this.state.getWebSockets();

    await Promise.allSettled(
      sockets.map(async (ws) => {
        try {
          const nodeId = (ws as unknown as { deserializeAttachment(): unknown }).deserializeAttachment() as string;
          const netmap = await buildNetMap(this.env, nodeId);
          ws.send(JSON.stringify(netmap));
        } catch (err) {
          ws.close(1011, "failed to build netmap");
        }
      })
    );
  }
}

// GET /api/topology/ws
// Proxies the WebSocket upgrade into the singleton TopologyHub DO.
// Forwards X-Node-ID so the DO can tag the session.
// TODO: validate that the nodeId in X-Node-ID matches the Zero Trust JWT once auth is wired up.
export async function handleTopologyWS(req: Request, env: Env): Promise<Response> {
  const nodeId = req.headers.get("X-Node-ID");
  if (!nodeId) {
    return Response.json({ error: "X-Node-ID header required" }, { status: 401 });
  }

  // Rewrite the URL to the DO's internal /ws route
  const doUrl = new URL(req.url);
  doUrl.pathname = "/ws";

  const hub = env.TOPOLOGY_HUB.get(env.TOPOLOGY_HUB.idFromName("global"));
  return hub.fetch(new Request(doUrl.toString(), req));
}
