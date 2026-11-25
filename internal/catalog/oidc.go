package catalog

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/mail"
	"slices"
	"strings"

	"forgetmenote/internal/workspace"
)

type ExternalIdentity struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
	User    string `json:"user"`
	// Email and HostedDomain are the latest sign-in's provider-verified address
	// and Google Workspace domain, lowercased. They pair messaging users by email
	// (see PairMessageUsersByEmail); they never identify, claim or merge accounts.
	Email        string `json:"email,omitempty"`
	HostedDomain string `json:"hostedDomain,omitempty"`
}

// ObserveOIDCEmail records the verified address and hosted domain from the
// identity's latest sign-in, or clears them when the provider no longer
// verifies one. An unchanged sign-in writes nothing.
func (c *Catalog) ObserveOIDCEmail(issuer, sub, verifiedEmail, hostedDomain string) error {
	email := strings.ToLower(validEmail(verifiedEmail))
	domain := strings.ToLower(strings.TrimSpace(hostedDomain))
	if email == "" || !validHostedDomain(domain) {
		domain = ""
	}
	c.lock()
	defer c.mu.Unlock()
	if c.version < 0 {
		return ErrStorageUnavailable
	}
	i := slices.IndexFunc(c.state.OIDC, func(v ExternalIdentity) bool { return v.Issuer == issuer && v.Subject == sub })
	if i < 0 {
		return workspace.ErrForbidden
	}
	v := c.state.OIDC[i]
	if v.Email == email && v.HostedDomain == domain {
		return nil
	}
	return c.change(v.User, "account.external.email", v.User, nil, func() error {
		c.state.OIDC[i].Email, c.state.OIDC[i].HostedDomain = email, domain
		return nil
	})
}

// validEmail returns a plain provider address, or "" for anything else.
func validEmail(raw string) string {
	if len(raw) > 120 || strings.ContainsAny(raw, "\r\n\x00") {
		return ""
	}
	raw = strings.TrimSpace(raw)
	if address, err := mail.ParseAddress(raw); err != nil || address.Address != raw {
		return ""
	}
	return raw
}

func validHostedDomain(domain string) bool {
	if domain == "" || len(domain) > 253 || !strings.Contains(domain, ".") {
		return false
	}
	for _, r := range domain {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-') {
			return false
		}
	}
	return true
}

// ResolveOIDC accepts only a verified issuer and subject. A nonempty link is the
// currently authenticated principal, never a username or email claim from the provider.
// verifiedEmail must be an address the provider asserted with email_verified=true
// (empty otherwise). It supplies a default display name only; it never identifies
// or merges accounts.
func (c *Catalog) ResolveOIDC(issuer, sub, link, verifiedEmail string, groups ...string) (string, error) {
	c.lock()
	defer c.mu.Unlock()
	if c.version < 0 {
		return "", workspace.ErrForbidden
	}
	if issuer == "" || sub == "" || len(sub) > 1024 {
		return "", errors.New("invalid external identity")
	}
	verifiedEmail = validEmail(verifiedEmail)
	for _, v := range c.state.OIDC {
		if v.Issuer == issuer && v.Subject == sub {
			user := c.state.Users[v.User]
			if user.Disabled || user.Kind == "service" {
				return "", workspace.ErrForbidden
			}
			if link != "" && link != v.User {
				return "", errors.New("external identity already belongs to another account")
			}
			if user.DisplayName == "" && verifiedEmail != "" {
				user.DisplayName = verifiedEmail
				return v.User, c.change(v.User, "account.external", v.User, nil, func() error {
					c.state.Users[v.User] = user
					return nil
				})
			}
			return v.User, nil
		}
	}
	id := link
	if id != "" {
		if !ValidUser(id) || c.state.Users[id].Disabled || c.state.Users[id].Kind == "service" {
			return "", workspace.ErrForbidden
		}
		if _, ok := c.state.Accounts[id]; !ok {
			return "", workspace.ErrForbidden
		}
	} else {
		for _, group := range groups {
			g, exists := c.state.Groups[group]
			if !exists || len(g.Members) >= 4096 {
				return "", errors.New("OIDC default group is missing or full; contact the administrator")
			}
		}
		raw := make([]byte, 16)
		rand.Read(raw)
		id = "oidc-" + hex.EncodeToString(raw)
	}
	err := c.change(id, "account.external", id, nil, func() error {
		user, exists := c.state.Users[id]
		if !exists {
			user = User{ID: id}
		}
		if user.DisplayName == "" {
			user.DisplayName = verifiedEmail
		}
		c.state.Users[id] = user
		c.state.OIDC = append(c.state.OIDC, ExternalIdentity{Issuer: issuer, Subject: sub, User: id})
		// Defaults apply only at provisioning. Later logins and account linking
		// must not restore membership an administrator deliberately removed.
		if link == "" {
			for _, group := range groups {
				g := c.state.Groups[group]
				g.Members = append(slices.Clone(g.Members), id)
				slices.Sort(g.Members)
				g.Members = slices.Compact(g.Members)
				g.Revision = c.state.ActivitySequence + 1
				c.state.Groups[group] = g
			}
		}
		return nil
	})
	return id, err
}
func (c *Catalog) ExternalIdentities(id string) []ExternalIdentity {
	c.lock()
	defer c.mu.Unlock()
	out := []ExternalIdentity{}
	for _, v := range c.state.OIDC {
		if v.User == id {
			out = append(out, v)
		}
	}
	return out
}
func (c *Catalog) Epoch(id string) uint64 {
	c.lock()
	defer c.mu.Unlock()
	return c.state.AccessEpoch[id]
}

var ErrStorageUnavailable = errors.New("catalog authority unavailable; retry shortly")

func (c *Catalog) CheckSession(id string, epoch uint64) error {
	c.lock()
	defer c.mu.Unlock()
	if c.version < 0 {
		return ErrStorageUnavailable
	}
	if (id == Bootstrap || ValidUser(id) && !c.state.Users[id].Disabled) && c.state.AccessEpoch[id] == epoch {
		return nil
	}
	return workspace.ErrForbidden
}
func (c *Catalog) ValidSession(id string, epoch uint64) bool { return c.CheckSession(id, epoch) == nil }
func (c *Catalog) Revoke(actor, id string) error {
	c.lock()
	defer c.mu.Unlock()
	if actor != id && !c.admin(actor) {
		return workspace.ErrForbidden
	}
	if id != Bootstrap && !ValidUser(id) {
		return workspace.ErrForbidden
	}
	return c.change(actor, "sessions.revoke", id, nil, func() error { c.state.AccessEpoch[id]++; return nil })
}

// Startup seeding creates absent proxy users only; operator changes always win.
func (c *Catalog) SeedAdmins(ids []string) error {
	c.lock()
	defer c.mu.Unlock()
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if !ValidUser(id) {
			return errors.New("invalid administrator ID")
		}
		if _, ok := c.state.Users[id]; ok {
			continue
		}
		if err := c.change(Bootstrap, "account.seed_admin", id, nil, func() error {
			c.state.Users[id] = User{ID: id, Admin: true}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// GrantAdmin is an explicit offline operator action on an already provisioned user.
func (c *Catalog) GrantAdmin(id string) error {
	c.lock()
	defer c.mu.Unlock()
	u, ok := c.state.Users[id]
	if !ok || u.Disabled || u.Kind == "service" {
		return errors.New("administrator must already exist and be enabled")
	}
	u.Admin = true
	return c.change(Bootstrap, "account.grant_admin", id, nil, func() error {
		c.state.Users[id] = u
		return nil
	})
}
