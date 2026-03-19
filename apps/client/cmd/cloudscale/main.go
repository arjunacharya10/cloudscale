package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/cloudscale/client/internal/controlclient"
	"github.com/cloudscale/client/internal/endpoint"
	"github.com/cloudscale/client/internal/netmap"
	"github.com/cloudscale/client/internal/wireguard"
)

// --- Config / State ---

// Config is loaded from ~/.config/cloudscale/config.json.
type Config struct {
	ControlURL string `json:"controlURL"` // e.g. "https://cloudscale.example.com"
	NetworkKey string `json:"networkKey"` // shared secret
	NodeName   string `json:"nodeName"`   // human-readable name for this machine
	UserID     string `json:"userId"`     // arbitrary label (e.g. "alice")
}

// State persists the registered nodeID, mesh IP, and WireGuard keypair
// to ~/.local/share/cloudscale/state.json.
type State struct {
	NodeID     string `json:"nodeId"`
	MeshIP     string `json:"meshIp"`
	PrivateKey string `json:"privateKey"`
	PublicKey  string `json:"publicKey"`
}

func configPath() string {
	base, _ := os.UserConfigDir()
	return filepath.Join(base, "cloudscale", "config.json")
}

func statePath() string {
	base, _ := os.UserHomeDir()
	return filepath.Join(base, ".local", "share", "cloudscale", "state.json")
}

func loadConfig() (*Config, error) {
	path := configPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if cfg.ControlURL == "" || cfg.NetworkKey == "" || cfg.NodeName == "" {
		return nil, fmt.Errorf("config must set controlURL, networkKey, and nodeName")
	}
	return &cfg, nil
}

func loadState() (*State, error) {
	data, err := os.ReadFile(statePath())
	if err != nil {
		return nil, err
	}
	var s State
	return &s, json.Unmarshal(data, &s)
}

func saveState(s *State) error {
	path := statePath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func removeState() {
	os.Remove(statePath()) //nolint:errcheck
}

// --- Commands ---

func main() {
	root := &cobra.Command{
		Use:   "cloudscale",
		Short: "Cloudscale mesh VPN client",
	}
	root.AddCommand(upCmd(), downCmd(), statusCmd())
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

// cloudscale up — register (or reuse existing registration), bring up WireGuard, run daemon.
func upCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "up",
		Short: "Connect to the mesh",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			cc := controlclient.New(controlclient.Config{
				ControlURL: cfg.ControlURL,
				NetworkKey: cfg.NetworkKey,
			})

			// Register or load existing state.
			state, err := ensureRegistered(cfg, cc)
			if err != nil {
				return err
			}

			// Bring up WireGuard interface and add mesh route.
			if err := wireguard.EnsureInterface(state.MeshIP + "/10"); err != nil {
				return fmt.Errorf("bring up interface: %w", err)
			}

			wg, err := wireguard.New()
			if err != nil {
				return fmt.Errorf("wgctrl: %w", err)
			}
			defer wg.Close()

			if err := wg.Configure(state.PrivateKey); err != nil {
				return fmt.Errorf("configure wireguard: %w", err)
			}

			// Apply initial netmap.
			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			if nm, err := cc.GetNetMap(ctx, state.NodeID); err == nil {
				peers := netmap.ToWireGuardPeers(*nm)
				wg.SyncPeers(peers) //nolint:errcheck
			}

			fmt.Printf("cloudscale up — %s (%s)\n", cfg.NodeName, state.MeshIP)

			// Send an initial heartbeat immediately so our endpoints are registered
			// before other nodes try to reach us.
			eps := endpoint.Discover(wireguard.ListenPort)
			if hbCtx, cancel := context.WithTimeout(ctx, 10*time.Second); true {
				cc.Heartbeat(hbCtx, state.NodeID, eps) //nolint:errcheck
				cancel()
			}

			// Run heartbeat + WebSocket in the background.
			go runHeartbeat(ctx, cc, state.NodeID)
			go runWebSocket(ctx, cc, wg, state.NodeID)

			<-ctx.Done()
			fmt.Println("\nshutting down…")
			return nil
		},
	}
}

// cloudscale down — deregister this node and bring down the interface.
func downCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "Disconnect from the mesh and deregister this node",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			state, err := loadState()
			if err != nil {
				return fmt.Errorf("not connected (no state file): %w", err)
			}

			cc := controlclient.New(controlclient.Config{
				ControlURL: cfg.ControlURL,
				NetworkKey: cfg.NetworkKey,
			})

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			if err := cc.Deregister(ctx, state.NodeID); err != nil {
				fmt.Fprintf(os.Stderr, "warn: deregister: %v\n", err)
			}

			wireguard.TeardownInterface() //nolint:errcheck
			removeState()
			fmt.Println("cloudscale down")
			return nil
		},
	}
}

// cloudscale status — print current node info and peer list.
func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show mesh status for this node",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			state, err := loadState()
			if err != nil {
				return fmt.Errorf("not connected (no state file)")
			}

			cc := controlclient.New(controlclient.Config{
				ControlURL: cfg.ControlURL,
				NetworkKey: cfg.NetworkKey,
			})

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			nm, err := cc.GetNetMap(ctx, state.NodeID)
			if err != nil {
				return fmt.Errorf("fetch netmap: %w", err)
			}

			fmt.Printf("self:  %s  %s  (key: %s…)\n",
				cfg.NodeName, nm.Self.Addresses[0], nm.Self.PublicKey[:8])
			fmt.Printf("\npeers (%d):\n", len(nm.Peers))
			for _, p := range nm.Peers {
				status := "offline"
				if p.Online {
					status = "online"
				}
				ep := "(no endpoint)"
				if len(p.Endpoints) > 0 {
					ep = p.Endpoints[0]
				}
				fmt.Printf("  %-20s  %-15s  %-7s  %s\n",
					p.Name, p.Addresses[0], status, ep)
			}
			return nil
		},
	}
}

// --- Helpers ---

// ensureRegistered loads existing state or registers the node if none exists.
func ensureRegistered(cfg *Config, cc *controlclient.Client) (*State, error) {
	if state, err := loadState(); err == nil {
		fmt.Printf("reusing existing registration (nodeId: %s)\n", state.NodeID)
		return state, nil
	}

	// Generate a new WireGuard keypair.
	privateKey, err := wireguard.GeneratePrivateKey()
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	publicKey, err := wireguard.PublicKeyFromPrivate(privateKey)
	if err != nil {
		return nil, fmt.Errorf("derive public key: %w", err)
	}

	userID := cfg.UserID
	if userID == "" {
		userID = cfg.NodeName
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	resp, err := cc.Register(ctx, cfg.NodeName, publicKey, userID)
	if err != nil {
		return nil, fmt.Errorf("register: %w", err)
	}

	state := &State{
		NodeID:     resp.NodeID,
		MeshIP:     resp.MeshIP,
		PrivateKey: privateKey,
		PublicKey:  publicKey,
	}
	if err := saveState(state); err != nil {
		return nil, fmt.Errorf("save state: %w", err)
	}

	fmt.Printf("registered as %s, mesh IP: %s\n", cfg.NodeName, resp.MeshIP)
	return state, nil
}

// runHeartbeat sends a heartbeat every 30 seconds until ctx is done.
func runHeartbeat(ctx context.Context, cc *controlclient.Client, nodeID string) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hbCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			eps := endpoint.Discover(wireguard.ListenPort)
			cc.Heartbeat(hbCtx, nodeID, eps) //nolint:errcheck
			cancel()
		}
	}
}

// runWebSocket connects to the topology WebSocket and applies netmap updates.
// Reconnects with backoff on error until ctx is cancelled.
func runWebSocket(ctx context.Context, cc *controlclient.Client, wg *wireguard.Manager, nodeID string) {
	backoff := 2 * time.Second
	for {
		err := cc.ConnectWS(ctx, nodeID, func(nm controlclient.NetMap) {
			peers := netmap.ToWireGuardPeers(nm)
			if err := wg.SyncPeers(peers); err != nil {
				fmt.Fprintf(os.Stderr, "sync peers: %v\n", err)
			}
		})
		if ctx.Err() != nil {
			return
		}
		fmt.Fprintf(os.Stderr, "ws disconnected (%v), reconnecting in %s\n", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
	}
}
