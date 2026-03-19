// Package netmap parses the network map from the control plane
// and translates it into WireGuard peer configuration.
package netmap

import (
	"github.com/cloudscale/client/internal/controlclient"
	"github.com/cloudscale/client/internal/wireguard"
)

// ToWireGuardPeers converts a control-plane NetMap into the WireGuard peer
// list needed by wireguard.Manager.SyncPeers. Only online peers are included
// — offline peers have no endpoint and would stall the handshake.
func ToWireGuardPeers(nm controlclient.NetMap) []wireguard.PeerConfig {
	peers := make([]wireguard.PeerConfig, 0, len(nm.Peers))
	for _, p := range nm.Peers {
		if !p.Online {
			continue
		}
		peers = append(peers, wireguard.PeerConfig{
			PublicKey:  p.PublicKey,
			Endpoints:  p.Endpoints,
			AllowedIPs: p.AllowedIPs,
		})
	}
	return peers
}
