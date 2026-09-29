package siding

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
)

// parseTrustedSources reads trustedSources entries: a CIDR, or a bare address
// standing for itself.
func parseTrustedSources(entries []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(entries))
	for i, entry := range entries {
		prefix, err := parseTrustedSource(strings.TrimSpace(entry))
		if err != nil {
			return nil, fmt.Errorf("trustedSources[%d] %q: %w", i, entry, err)
		}
		out = append(out, prefix)
	}
	return out, nil
}

func parseTrustedSource(entry string) (netip.Prefix, error) {
	const forms = `use a CIDR ("10.42.0.0/16") or an address ("192.168.1.10")`
	if entry == "" {
		return netip.Prefix{}, errors.New("empty entry; " + forms)
	}
	var prefix netip.Prefix
	if strings.Contains(entry, "/") {
		p, err := netip.ParsePrefix(entry)
		if err != nil {
			return netip.Prefix{}, errors.New("not a CIDR; " + forms)
		}
		prefix = p
	} else {
		addr, err := netip.ParseAddr(entry)
		if err != nil {
			return netip.Prefix{}, errors.New("not an address; " + forms)
		}
		prefix = netip.PrefixFrom(addr, addr.BitLen())
	}
	addr := prefix.Addr()
	if addr.Is4In6() {
		// Peers are unmapped before matching, so this form would never match.
		return netip.Prefix{}, fmt.Errorf("IPv4-mapped IPv6; write the IPv4 form (%s)", addr.Unmap())
	}
	if addr.Zone() != "" {
		return netip.Prefix{}, errors.New("zones are not supported")
	}
	return prefix.Masked(), nil
}

// trustedPeer reports whether routing baggage from req's peer is accepted.
// Only the connection's own address counts: X-Forwarded-For is whatever the
// client wrote. With no trustedSources every peer is trusted.
func (r *Router) trustedPeer(req *http.Request) bool {
	if len(r.trusted) == 0 {
		return true
	}
	peer, err := netip.ParseAddrPort(req.RemoteAddr)
	if err != nil {
		return false
	}
	addr := peer.Addr().Unmap().WithZone("")
	for _, prefix := range r.trusted {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}
