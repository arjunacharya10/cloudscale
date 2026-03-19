// Package endpoint discovers the endpoints this node should advertise to peers.
// It returns a prioritized list: public IP (via STUN) first, then local IPs.
//
// STUN note: we open a temporary UDP connection to the STUN server (from a
// random port) to learn our public IP, then pair it with the WireGuard listen
// port. This is accurate for endpoint-independent NATs (the common case). For
// symmetric NATs, the port mapping may differ — DERP relay handles that case.
package endpoint

import (
	"fmt"
	"net"
	"time"

	"github.com/pion/stun"
)

const (
	stunServer = "stun.cloudflare.com:3478"
	stunTimeout = 5 * time.Second
)

// Discover returns all endpoints this node should advertise, in priority order:
//  1. Public IP:wgPort (via STUN)
//  2. LAN IP:wgPort (non-loopback, non-mesh interfaces)
//
// Never returns an error — partial results are better than none.
func Discover(wgPort int) []string {
	var endpoints []string

	if ip, err := publicIP(); err == nil {
		endpoints = append(endpoints, net.JoinHostPort(ip, fmt.Sprintf("%d", wgPort)))
	}

	endpoints = append(endpoints, localEndpoints(wgPort)...)
	return endpoints
}

// publicIP contacts a STUN server and returns the public IP of this host.
func publicIP() (string, error) {
	conn, err := net.DialTimeout("udp", stunServer, stunTimeout)
	if err != nil {
		return "", fmt.Errorf("dial stun: %w", err)
	}
	conn.SetDeadline(time.Now().Add(stunTimeout)) //nolint:errcheck

	c, err := stun.NewClient(conn)
	if err != nil {
		conn.Close()
		return "", fmt.Errorf("stun client: %w", err)
	}
	defer c.Close()

	msg, err := stun.Build(stun.TransactionID, stun.BindingRequest)
	if err != nil {
		return "", err
	}

	var xorAddr stun.XORMappedAddress
	var doErr error
	if err := c.Do(msg, func(e stun.Event) {
		if e.Error != nil {
			doErr = e.Error
			return
		}
		doErr = xorAddr.GetFrom(e.Message)
	}); err != nil {
		return "", err
	}
	if doErr != nil {
		return "", doErr
	}

	return xorAddr.IP.String(), nil // caller wraps with net.JoinHostPort
}

// localEndpoints returns host:wgPort for each non-loopback, non-mesh IPv4 address.
func localEndpoints(wgPort int) []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}

	_, meshNet, _ := net.ParseCIDR("100.64.0.0/10")

	var eps []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.To4() == nil || ip.IsLoopback() {
				continue
			}
			if meshNet.Contains(ip) {
				continue // skip our own mesh IP
			}
			eps = append(eps, net.JoinHostPort(ip.String(), fmt.Sprintf("%d", wgPort)))
		}
	}
	return eps
}
