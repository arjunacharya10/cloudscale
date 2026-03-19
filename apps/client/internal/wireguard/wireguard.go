// Package wireguard manages the local WireGuard interface.
// Responsibilities: interface creation, peer configuration, key management.
//
// Interface creation is OS-specific and handled via os/exec before calling
// Configure/SyncPeers. The Manager uses wgctrl to configure an existing
// WireGuard interface via the kernel or userspace WireGuard daemon.
package wireguard

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

const (
	InterfaceName = "cloudscale0"
	ListenPort    = 51820
)

// PeerConfig holds the WireGuard-level config for one peer.
type PeerConfig struct {
	PublicKey  string
	Endpoints  []string // try in order; first resolvable wins
	AllowedIPs []string // CIDR strings, e.g. "100.64.0.2/32"
}

// Manager configures a WireGuard interface via wgctrl.
// Call EnsureInterface first to create the OS-level interface if needed.
type Manager struct {
	client *wgctrl.Client
	ifName string
}

// New opens a wgctrl client. The caller must call Close when done.
func New() (*Manager, error) {
	c, err := wgctrl.New()
	if err != nil {
		return nil, fmt.Errorf("wgctrl: %w", err)
	}
	return &Manager{client: c, ifName: InterfaceName}, nil
}

// Close releases the wgctrl client.
func (m *Manager) Close() error {
	return m.client.Close()
}

// GeneratePrivateKey generates a new WireGuard private key.
func GeneratePrivateKey() (string, error) {
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return "", err
	}
	return key.String(), nil
}

// PublicKeyFromPrivate derives the public key from a base64-encoded private key.
func PublicKeyFromPrivate(privateKeyStr string) (string, error) {
	key, err := wgtypes.ParseKey(privateKeyStr)
	if err != nil {
		return "", fmt.Errorf("parse private key: %w", err)
	}
	return key.PublicKey().String(), nil
}

// EnsureInterface creates the WireGuard interface and assigns meshIP if it
// does not already exist. meshIP should be in CIDR notation, e.g. "100.64.0.1/10".
// This is a best-effort call — if the interface already exists, the error is ignored.
func EnsureInterface(meshIP string) error {
	switch runtime.GOOS {
	case "linux":
		return ensureLinux(meshIP)
	case "darwin":
		return ensureDarwin(meshIP)
	default:
		return fmt.Errorf("unsupported OS: %s — create interface %q manually", runtime.GOOS, InterfaceName)
	}
}

// TeardownInterface brings down and removes the WireGuard interface.
func TeardownInterface() error {
	switch runtime.GOOS {
	case "linux":
		run("ip", "route", "del", meshCIDR, "dev", InterfaceName) //nolint:errcheck
		return run("ip", "link", "delete", InterfaceName)
	case "darwin":
		run("route", "delete", "-net", meshCIDR) //nolint:errcheck
		return run("wireguard-go", "--terminate", InterfaceName)
	default:
		return fmt.Errorf("unsupported OS: %s — remove interface %q manually", runtime.GOOS, InterfaceName)
	}
}

// Configure sets the private key and listen port on the interface.
func (m *Manager) Configure(privateKeyStr string) error {
	privateKey, err := wgtypes.ParseKey(privateKeyStr)
	if err != nil {
		return fmt.Errorf("parse private key: %w", err)
	}
	port := ListenPort
	return m.client.ConfigureDevice(m.ifName, wgtypes.Config{
		PrivateKey: &privateKey,
		ListenPort: &port,
	})
}

// SyncPeers replaces the full peer list on the interface.
// Peers not present in the new list are removed.
func (m *Manager) SyncPeers(peers []PeerConfig) error {
	var wgPeers []wgtypes.PeerConfig

	for _, p := range peers {
		pubKey, err := wgtypes.ParseKey(p.PublicKey)
		if err != nil {
			return fmt.Errorf("parse peer public key %q: %w", p.PublicKey, err)
		}

		var allowedIPs []net.IPNet
		for _, cidr := range p.AllowedIPs {
			_, ipNet, err := net.ParseCIDR(cidr)
			if err != nil {
				return fmt.Errorf("parse allowed IP %q: %w", cidr, err)
			}
			allowedIPs = append(allowedIPs, *ipNet)
		}

		// Use the first resolvable endpoint.
		var endpoint *net.UDPAddr
		for _, ep := range p.Endpoints {
			addr, err := net.ResolveUDPAddr("udp", ep)
			if err == nil {
				endpoint = addr
				break
			}
		}

		keepalive := 25 * time.Second
		wgPeers = append(wgPeers, wgtypes.PeerConfig{
			PublicKey:                   pubKey,
			Endpoint:                    endpoint,
			AllowedIPs:                  allowedIPs,
			ReplaceAllowedIPs:           true,
			PersistentKeepaliveInterval: &keepalive,
		})
	}

	return m.client.ConfigureDevice(m.ifName, wgtypes.Config{
		Peers:        wgPeers,
		ReplacePeers: true,
	})
}

// --- OS-specific interface creation ---

const meshCIDR = "100.64.0.0/10"

func ensureLinux(meshIP string) error {
	run("ip", "link", "add", "dev", InterfaceName, "type", "wireguard") //nolint:errcheck
	run("ip", "addr", "add", meshIP, "dev", InterfaceName)              //nolint:errcheck
	if err := run("ip", "link", "set", "up", "dev", InterfaceName); err != nil {
		return err
	}
	// Route the entire mesh CIDR through this interface.
	// "proto static" marks it as manually managed so it survives metric changes.
	run("ip", "route", "del", meshCIDR, "dev", InterfaceName)                              //nolint:errcheck
	return run("ip", "route", "add", meshCIDR, "dev", InterfaceName, "proto", "static")
}

func ensureDarwin(meshIP string) error {
	// wireguard-go starts a userspace WireGuard daemon and creates a utun interface.
	// wgctrl talks to it via /var/run/wireguard/<InterfaceName>.sock.
	run("wireguard-go", InterfaceName) //nolint:errcheck

	// The actual utun interface name (e.g. "utun5") is written to
	// /var/run/wireguard/<InterfaceName>.name once the daemon is ready.
	utun, err := darwinUtunName()
	if err != nil {
		return fmt.Errorf("waiting for wireguard-go: %w", err)
	}

	run("ifconfig", utun, "inet", meshIP, meshIP) //nolint:errcheck
	if err := run("ifconfig", utun, "up"); err != nil {
		return err
	}
	run("route", "delete", "-net", meshCIDR)                                    //nolint:errcheck
	return run("route", "add", "-net", meshCIDR, "-interface", utun)
}

// darwinUtunName reads the actual utun interface name that wireguard-go
// assigned, retrying briefly while the daemon starts up.
func darwinUtunName() (string, error) {
	namePath := fmt.Sprintf("/var/run/wireguard/%s.name", InterfaceName)
	for range 20 {
		data, err := os.ReadFile(namePath)
		if err == nil {
			if name := strings.TrimSpace(string(data)); name != "" {
				return name, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", fmt.Errorf("timed out waiting for %s", namePath)
}

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w\n%s", name, args, err, out)
	}
	return nil
}
