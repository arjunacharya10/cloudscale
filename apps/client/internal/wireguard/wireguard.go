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
	InterfaceName      = "cloudscale0" // used on Linux
	darwinSocketName   = "utun"        // wireguard-go socket name on macOS
	ListenPort         = 51820
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

// New opens a wgctrl client for the given interface name.
// Pass the value returned by EnsureInterface.
func New(ifName string) (*Manager, error) {
	c, err := wgctrl.New()
	if err != nil {
		return nil, fmt.Errorf("wgctrl: %w", err)
	}
	return &Manager{client: c, ifName: ifName}, nil
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

// EnsureInterface creates the WireGuard interface, assigns meshIP, and adds
// the mesh route. Returns the actual OS interface name to pass to New().
// meshIP should be in CIDR notation, e.g. "100.64.0.1/10".
func EnsureInterface(meshIP string) (string, error) {
	switch runtime.GOOS {
	case "linux":
		return InterfaceName, ensureLinux(meshIP)
	case "darwin":
		return ensureDarwin(meshIP)
	default:
		return "", fmt.Errorf("unsupported OS: %s — create interface %q manually", runtime.GOOS, InterfaceName)
	}
}

// TeardownInterface brings down and removes the WireGuard interface.
// Pass the ifName returned by EnsureInterface.
func TeardownInterface(ifName string) error {
	switch runtime.GOOS {
	case "linux":
		run("ip", "route", "del", meshCIDR, "dev", ifName) //nolint:errcheck
		return run("ip", "link", "delete", ifName)
	case "darwin":
		run("route", "delete", "-net", meshCIDR) //nolint:errcheck
		sockPath := fmt.Sprintf("/var/run/wireguard/%s.sock", ifName)
		os.Remove(sockPath)                      //nolint:errcheck
		return run("pkill", "-f", "wireguard-go")
	default:
		return fmt.Errorf("unsupported OS: %s — remove interface %q manually", runtime.GOOS, ifName)
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

func ensureDarwin(meshIP string) (string, error) {
	// Snapshot existing utun interfaces so we can detect the new one.
	before := utunInterfaces()

	// wireguard-go picks the next free utunN and creates a socket at
	// /var/run/wireguard/<utunN>.sock — wgctrl uses the utunN name directly.
	if err := run("wireguard-go", darwinSocketName); err != nil {
		return "", fmt.Errorf("wireguard-go: %w (is wireguard-go installed and in PATH?)", err)
	}

	// Wait for the new utunN interface to appear.
	utun, err := darwinNewUtun(before)
	if err != nil {
		return "", fmt.Errorf("waiting for wireguard-go interface: %w", err)
	}

	// Wait for the wgctrl socket (<utunN>.sock) to be ready.
	sockPath := fmt.Sprintf("/var/run/wireguard/%s.sock", utun)
	for range 20 {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Configure the interface address. macOS utun interfaces are point-to-point;
	// set the address and peer-address both to the mesh IP with explicit /32 netmask.
	ip, _, err := net.ParseCIDR(meshIP)
	if err != nil {
		return "", fmt.Errorf("parse mesh IP %q: %w", meshIP, err)
	}
	run("ifconfig", utun, "inet", ip.String(), ip.String(), "netmask", "255.255.255.255") //nolint:errcheck
	if err := run("ifconfig", utun, "up"); err != nil {
		return "", err
	}
	run("route", "delete", "-net", meshCIDR)                                 //nolint:errcheck
	if err := run("route", "add", "-net", meshCIDR, "-interface", utun); err != nil {
		return "", err
	}

	return utun, nil
}

// utunInterfaces returns the set of utunN interface names currently present.
func utunInterfaces() map[string]bool {
	ifaces, _ := net.Interfaces()
	names := make(map[string]bool, len(ifaces))
	for _, iface := range ifaces {
		if strings.HasPrefix(iface.Name, "utun") {
			names[iface.Name] = true
		}
	}
	return names
}

// darwinNewUtun polls until a utunN interface appears that wasn't in before.
func darwinNewUtun(before map[string]bool) (string, error) {
	for range 20 {
		for name := range utunInterfaces() {
			if !before[name] {
				return name, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", fmt.Errorf("no new utun interface appeared after 2s")
}

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w\n%s", name, args, err, out)
	}
	return nil
}
