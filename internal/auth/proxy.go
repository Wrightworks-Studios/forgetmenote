package auth

import (
	"crypto/subtle"
	"errors"
	"forgetmenote/internal/catalog"
	"net"
	"net/netip"
	"strings"
)

// Proxy owns the explicitly trusted socket peers and server-held SSO secret.
type Proxy struct {
	secret string
	peers  []netip.Prefix
}

func NewProxy(secret string, cidrs []string) (*Proxy, error) {
	if len(secret) < 32 {
		return nil, errors.New("proxy secret must contain at least 32 characters")
	}
	peers := []netip.Prefix{}
	for _, raw := range cidrs {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil {
			return nil, err
		}
		peers = append(peers, prefix)
	}
	if len(peers) == 0 {
		return nil, errors.New("trusted proxy addresses are required")
	}
	return &Proxy{secret, peers}, nil
}
func (p *Proxy) Identity(peer, secret, user string) string {
	if p == nil || subtle.ConstantTimeCompare([]byte(secret), []byte(p.secret)) != 1 {
		return ""
	}
	host, _, err := net.SplitHostPort(peer)
	if err != nil {
		return ""
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return ""
	}
	for _, prefix := range p.peers {
		if prefix.Contains(addr.Unmap()) && catalog.ValidUser(user) {
			return user
		}
	}
	return ""
}
