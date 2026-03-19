export interface NodeRow {
  node_id: string;
  node_name: string;
  user_id: string;
  public_key: string;
  mesh_ip: string;
  created_at: number;
  last_seen: number;
}

export interface EndpointRow {
  id: number;
  node_id: string;
  endpoint: string;
  updated_at: number;
}

export interface PeerInfo {
  nodeId: string;
  name: string;
  addresses: string[];
  publicKey: string;
  endpoints: string[];
  allowedIPs: string[];
  online: boolean;
}

export interface NetMap {
  self: {
    nodeId: string;
    user: string;
    addresses: string[];
    publicKey: string;
  };
  peers: PeerInfo[];
}

export interface Env {
  cloudscale: D1Database;
  TOPOLOGY_HUB: DurableObjectNamespace;
  NETWORK_KEY: string;
}
