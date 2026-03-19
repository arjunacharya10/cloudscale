// Package controlclient handles communication with the cloudscale control plane.
// Responsibilities: registration, netmap polling/streaming, heartbeat.
package controlclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
)

// Config holds the parameters needed to talk to the control plane.
type Config struct {
	// ControlURL is the base URL of the control plane, e.g. "https://cloudscale.example.com".
	ControlURL string
	// NetworkKey is the shared secret sent as "Authorization: Bearer <key>" on every request.
	NetworkKey string
}

// Client is a stateless HTTP/WebSocket client for the cloudscale control plane.
// It does not manage state (nodeID, keypair) — callers are responsible for persisting that.
type Client struct {
	cfg  Config
	http *http.Client
}

// New returns a Client for the given config.
func New(cfg Config) *Client {
	return &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: 10 * time.Second},
	}
}

// --- Registration ---

// RegisterRequest is the body for POST /api/nodes/register.
type RegisterRequest struct {
	NodeName  string `json:"nodeName"`
	PublicKey string `json:"publicKey"`
	UserID    string `json:"userId"`
}

// RegisterResponse is the response from POST /api/nodes/register.
type RegisterResponse struct {
	NodeID   string `json:"nodeId"`
	NodeName string `json:"nodeName"`
	MeshIP   string `json:"meshIp"`
}

// Register registers this node with the control plane and returns the assigned nodeID and mesh IP.
func (c *Client) Register(ctx context.Context, nodeName, publicKey, userID string) (*RegisterResponse, error) {
	body := RegisterRequest{NodeName: nodeName, PublicKey: publicKey, UserID: userID}
	var resp RegisterResponse
	if err := c.do(ctx, http.MethodPost, "/api/nodes/register", "", body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Deregister removes this node from the control plane.
func (c *Client) Deregister(ctx context.Context, nodeID string) error {
	return c.do(ctx, http.MethodDelete, "/api/nodes/"+nodeID, nodeID, nil, nil)
}

// --- Netmap ---

// SelfInfo describes this node in the netmap.
type SelfInfo struct {
	NodeID    string   `json:"nodeId"`
	User      string   `json:"user"`
	Addresses []string `json:"addresses"`
	PublicKey string   `json:"publicKey"`
}

// PeerInfo describes a peer in the netmap.
type PeerInfo struct {
	NodeID     string   `json:"nodeId"`
	Name       string   `json:"name"`
	Addresses  []string `json:"addresses"`
	PublicKey  string   `json:"publicKey"`
	Endpoints  []string `json:"endpoints"`
	AllowedIPs []string `json:"allowedIPs"`
	Online     bool     `json:"online"`
}

// NetMap is the full network map returned by the control plane.
type NetMap struct {
	Self  SelfInfo   `json:"self"`
	Peers []PeerInfo `json:"peers"`
}

// GetNetMap fetches the current netmap for nodeID.
func (c *Client) GetNetMap(ctx context.Context, nodeID string) (*NetMap, error) {
	var nm NetMap
	if err := c.do(ctx, http.MethodGet, "/api/netmap", nodeID, nil, &nm); err != nil {
		return nil, err
	}
	return &nm, nil
}

// --- Heartbeat ---

type heartbeatRequest struct {
	Endpoints []string `json:"endpoints"`
}

// Heartbeat updates last_seen and replaces the endpoint list for nodeID.
func (c *Client) Heartbeat(ctx context.Context, nodeID string, endpoints []string) error {
	body := heartbeatRequest{Endpoints: endpoints}
	return c.do(ctx, http.MethodPost, "/api/nodes/"+nodeID+"/heartbeat", nodeID, body, nil)
}

// --- WebSocket ---

// ConnectWS opens a WebSocket to /api/topology/ws and calls onNetMap whenever the
// control plane pushes a netmap update. Blocks until ctx is cancelled or the
// connection drops, then returns an error.
func (c *Client) ConnectWS(ctx context.Context, nodeID string, onNetMap func(NetMap)) error {
	u, err := url.Parse(c.cfg.ControlURL + "/api/topology/ws")
	if err != nil {
		return fmt.Errorf("invalid control URL: %w", err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+c.cfg.NetworkKey)
	headers.Set("X-Node-ID", nodeID)

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.DialContext(ctx, u.String(), headers)
	if err != nil {
		return fmt.Errorf("ws dial: %w", err)
	}
	defer conn.Close()

	// Close the connection when ctx is cancelled.
	go func() {
		<-ctx.Done()
		conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		conn.Close()
	}()

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("ws read: %w", err)
		}
		var nm NetMap
		if err := json.Unmarshal(msg, &nm); err != nil {
			continue // ignore malformed frames
		}
		onNetMap(nm)
	}
}

// --- internal HTTP helper ---

// do performs an authenticated HTTP request.
// nodeID is optional — pass "" when no X-Node-ID is needed (e.g. register).
func (c *Client) do(ctx context.Context, method, path, nodeID string, reqBody, respBody any) error {
	var bodyReader io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.cfg.ControlURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.NetworkKey)
	if nodeID != "" {
		req.Header.Set("X-Node-ID", nodeID)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("http %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var apiErr struct {
			Error string `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&apiErr) //nolint:errcheck
		if apiErr.Error != "" {
			return fmt.Errorf("api error %d: %s", resp.StatusCode, apiErr.Error)
		}
		return fmt.Errorf("http %d: %s", resp.StatusCode, resp.Status)
	}

	if respBody != nil {
		if err := json.NewDecoder(resp.Body).Decode(respBody); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}

	return nil
}
