package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
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
	NodeID    string `json:"nodeId"`
	MeshIP    string `json:"meshIp"`
	PrivateKey string `json:"privateKey"`
	PublicKey  string `json:"publicKey"`
	IfName    string `json:"ifName,omitempty"` // actual OS interface name (e.g. utun7 on macOS)
}

func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp" // fallback so paths are at least valid
	}
	return home
}

func configPath() string {
	return filepath.Join(homeDir(), ".config", "cloudscale", "config.json")
}

func statePath() string {
	return filepath.Join(homeDir(), ".local", "share", "cloudscale", "state.json")
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
	root.AddCommand(setupCmd(), upCmd(), deleteCmd(), statusCmd())
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

// cloudscale setup — interactive config wizard + optional service installation.
func setupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Configure cloudscale interactively",
		RunE: func(cmd *cobra.Command, args []string) error {
			reader := bufio.NewReader(os.Stdin)

			prompt := func(label, fallback string) string {
				if fallback != "" {
					fmt.Printf("%s [%s]: ", label, fallback)
				} else {
					fmt.Printf("%s: ", label)
				}
				val, _ := reader.ReadString('\n')
				val = strings.TrimSpace(val)
				if val == "" {
					return fallback
				}
				return val
			}

			fmt.Println("Cloudscale setup")
			fmt.Println("----------------")

			controlURL := prompt("Control plane URL (e.g. https://cloudscale.example.workers.dev)", "")
			if controlURL == "" {
				return fmt.Errorf("controlURL is required")
			}

			networkKey := prompt("Network key (shared secret)", "")
			if networkKey == "" {
				return fmt.Errorf("networkKey is required")
			}

			hostname, _ := os.Hostname()
			nodeName := prompt("Node name", hostname)
			if nodeName == "" {
				return fmt.Errorf("nodeName is required")
			}

			userID := prompt("User ID", nodeName)

			cfg := Config{
				ControlURL: controlURL,
				NetworkKey: networkKey,
				NodeName:   nodeName,
				UserID:     userID,
			}

			path := configPath()
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				return fmt.Errorf("create config dir: %w", err)
			}
			data, err := json.MarshalIndent(cfg, "", "  ")
			if err != nil {
				return err
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				return fmt.Errorf("write config: %w", err)
			}
			fmt.Printf("\nConfig written to %s\n", path)

			// Optional service installation.
			fmt.Printf("\nInstall cloudscale as a system service? [y/N]: ")
			answer, _ := reader.ReadString('\n')
			if strings.ToLower(strings.TrimSpace(answer)) == "y" {
				exe, err := os.Executable()
				if err != nil {
					return fmt.Errorf("locate executable: %w", err)
				}
				if err := installService(exe); err != nil {
					return fmt.Errorf("install service: %w", err)
				}
			} else {
				fmt.Println("\nRun manually:")
				fmt.Println("  sudo cloudscale up")
			}

			return nil
		},
	}
}

// installService writes a systemd unit (Linux) or launchd plist (macOS).
func installService(exePath string) error {
	switch runtime.GOOS {
	case "linux":
		return installSystemd(exePath)
	case "darwin":
		return installLaunchd(exePath)
	default:
		return fmt.Errorf("service installation not supported on %s", runtime.GOOS)
	}
}

func installSystemd(exePath string) error {
	unit := fmt.Sprintf(`[Unit]
Description=Cloudscale mesh VPN
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s up
Restart=on-failure
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
`, exePath)

	const path = "/etc/systemd/system/cloudscale.service"
	if err := os.WriteFile(path, []byte(unit), 0644); err != nil {
		return fmt.Errorf("write unit file (run setup as root): %w", err)
	}
	fmt.Printf("Systemd unit written to %s\n\n", path)
	fmt.Println("Enable and start:")
	fmt.Println("  sudo systemctl daemon-reload")
	fmt.Println("  sudo systemctl enable --now cloudscale")
	fmt.Println("\nView logs:")
	fmt.Println("  sudo journalctl -u cloudscale -f")
	return nil
}

func installLaunchd(exePath string) error {
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.cloudscale</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>up</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/var/log/cloudscale.log</string>
    <key>StandardErrorPath</key>
    <string>/var/log/cloudscale.log</string>
</dict>
</plist>
`, exePath)

	const path = "/Library/LaunchDaemons/com.cloudscale.plist"
	if err := os.WriteFile(path, []byte(plist), 0644); err != nil {
		return fmt.Errorf("write plist (run setup as root): %w", err)
	}
	fmt.Printf("LaunchDaemon plist written to %s\n\n", path)
	fmt.Println("Enable and start:")
	fmt.Println("  sudo launchctl load /Library/LaunchDaemons/com.cloudscale.plist")
	fmt.Println("\nStop:")
	fmt.Println("  sudo launchctl unload /Library/LaunchDaemons/com.cloudscale.plist")
	fmt.Println("\nView logs:")
	fmt.Println("  tail -f /var/log/cloudscale.log")
	return nil
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
			ifName, err := wireguard.EnsureInterface(state.MeshIP + "/10")
			if err != nil {
				return fmt.Errorf("bring up interface: %w", err)
			}
			state.IfName = ifName
			saveState(state) //nolint:errcheck

			wg, err := wireguard.New(ifName)
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
			hbCtx, hbCancel := context.WithTimeout(ctx, 10*time.Second)
			cc.Heartbeat(hbCtx, state.NodeID, eps) //nolint:errcheck
			hbCancel()

			// Run heartbeat + WebSocket in the background.
			go runHeartbeat(ctx, cc, state.NodeID)
			go runWebSocket(ctx, cc, wg, state.NodeID)

			<-ctx.Done()
			fmt.Println("\nshutting down…")
			return nil
		},
	}
}

// cloudscale delete — deregister this node, wipe local state, and tear down the interface.
// Fresh keys and a new mesh IP will be assigned on the next cloudscale up.
func deleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete",
		Short: "Deregister this node and wipe local state (fresh start on next up)",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Deregister from control plane if we have state.
			state, stateErr := loadState()
			if stateErr == nil {
				cfg, err := loadConfig()
				if err == nil {
					cc := controlclient.New(controlclient.Config{
						ControlURL: cfg.ControlURL,
						NetworkKey: cfg.NetworkKey,
					})
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					if err := cc.Deregister(ctx, state.NodeID); err != nil {
						fmt.Fprintf(os.Stderr, "warn: deregister: %v\n", err)
					} else {
						fmt.Println("deregistered from control plane")
					}
					cancel()
				}
			}

			// Tear down the WireGuard interface.
			// Fall back to the default interface name if state is missing or IfName is empty.
			ifName := wireguard.InterfaceName
			if stateErr == nil && state.IfName != "" {
				ifName = state.IfName
			}
			if err := wireguard.TeardownInterface(ifName); err != nil {
				fmt.Fprintf(os.Stderr, "warn: teardown interface: %v\n", err)
			} else {
				fmt.Printf("interface %s removed\n", ifName)
			}

			// On macOS, clean up any leftover wireguard-go socket files.
			if runtime.GOOS == "darwin" {
				cleanupWireguardSockets()
			}

			removeState()
			fmt.Println("cloudscale down")
			return nil
		},
	}
}

// cleanupWireguardSockets removes stale wireguard-go socket files on macOS.
func cleanupWireguardSockets() {
	entries, err := os.ReadDir("/var/run/wireguard")
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sock") {
			os.Remove("/var/run/wireguard/" + e.Name()) //nolint:errcheck
		}
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
