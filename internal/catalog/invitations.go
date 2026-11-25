package catalog

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"forgetmenote/internal/workspace"
)

type InvitationSpec struct {
	Name     string            `json:"name"`
	Groups   []string          `json:"groups"`
	Projects map[string]string `json:"projects"`
}
type Invitation struct {
	InvitationSpec
	ID          string    `json:"id"`
	TokenHash   string    `json:"tokenHash,omitempty"`
	Issuer      string    `json:"issuer"`
	IssuerEpoch uint64    `json:"issuerEpoch"`
	Created     time.Time `json:"created"`
	Expires     time.Time `json:"expires"`
	Status      string    `json:"status"`
	User        string    `json:"user,omitempty"`
	Reset       bool      `json:"reset,omitempty"`
}
type IssuedInvitation struct {
	Invitation
	Token string `json:"token"`
}

func secretToken() string       { return rand.Text() + rand.Text() }
func tokenHash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func (c *Catalog) CreateInvitation(actor string, spec InvitationSpec, resetUser string) (IssuedInvitation, error) {
	c.lock()
	defer c.mu.Unlock()
	if !c.admin(actor) {
		return IssuedInvitation{}, workspace.ErrForbidden
	}
	if resetUser != "" {
		u, ok := c.state.Users[resetUser]
		found := false
		for _, credential := range c.state.Passwords {
			found = found || credential.User == resetUser
		}
		if !ok || u.Disabled || u.Kind == "service" || !found {
			return IssuedInvitation{}, workspace.ErrForbidden
		}
		spec = InvitationSpec{Name: u.DisplayName}
		if spec.Name == "" {
			spec.Name = resetUser
		}
	}
	if !validLabel(spec.Name) || len(spec.Groups) > 256 || len(spec.Projects) > 256 {
		return IssuedInvitation{}, errors.New("provide a name and valid invitation grants")
	}
	if err := c.validateInvitationGrants(spec); err != nil {
		return IssuedInvitation{}, err
	}
	token := secretToken()
	now := time.Now().UTC()
	expiry := now.Add(7 * 24 * time.Hour)
	if resetUser != "" {
		expiry = now.Add(time.Hour)
	}
	v := Invitation{InvitationSpec: InvitationSpec{Name: strings.TrimSpace(spec.Name), Groups: slices.Clone(spec.Groups), Projects: maps.Clone(spec.Projects)}, ID: secretToken(), TokenHash: tokenHash(token), Issuer: actor, IssuerEpoch: c.state.AccessEpoch[actor], Created: now, Expires: expiry, Status: "pending", Reset: resetUser != "", User: resetUser}
	err := c.change(actor, "invitation.create", v.ID, map[string]string{"user": resetUser}, func() error {
		for id, old := range c.state.Invitations {
			if old.Expires.Before(now.Add(-30 * 24 * time.Hour)) {
				delete(c.state.Invitations, id)
				continue
			}
			if v.Reset && old.Reset && old.User == v.User && old.Status == "pending" {
				old.Status = "revoked"
				old.TokenHash = ""
				c.state.Invitations[id] = old
			}
		}
		if len(c.state.Invitations) >= 4096 {
			return errors.New("invitation limit reached")
		}
		c.state.Invitations[v.ID] = v
		return nil
	})
	if err != nil {
		return IssuedInvitation{}, err
	}
	v.TokenHash = ""
	return IssuedInvitation{Invitation: v, Token: token}, nil
}
func (c *Catalog) validateInvitationGrants(spec InvitationSpec) error {
	for _, id := range spec.Groups {
		if _, ok := c.state.Groups[id]; !ok {
			return errors.New("invitation group no longer exists")
		}
	}
	for id, role := range spec.Projects {
		p, ok := c.state.Projects[id]
		if !ok || p.protected() || rank(role) < 1 || rank(role) > 3 {
			return errors.New("invitations may grant Reader, Commenter or Editor on shared notebooks")
		}
	}
	return nil
}
func (c *Catalog) RevokeInvitation(actor, id string) error {
	c.lock()
	defer c.mu.Unlock()
	if !c.admin(actor) {
		return workspace.ErrForbidden
	}
	if _, ok := c.state.Invitations[id]; !ok {
		return workspace.ErrForbidden
	}
	return c.change(actor, "invitation.revoke", id, nil, func() error {
		v := c.state.Invitations[id]
		v.Status = "revoked"
		v.TokenHash = ""
		c.state.Invitations[id] = v
		return nil
	})
}
func (c *Catalog) invitation(token string) (Invitation, error) {
	if len(token) < 32 || len(token) > 128 {
		return Invitation{}, workspace.ErrForbidden
	}
	hash := tokenHash(token)
	for _, v := range c.state.Invitations {
		if v.TokenHash == hash && v.Status == "pending" && time.Now().Before(v.Expires) && c.admin(v.Issuer) && c.state.AccessEpoch[v.Issuer] == v.IssuerEpoch {
			return v, nil
		}
	}
	return Invitation{}, workspace.ErrForbidden
}
func (c *Catalog) InspectInvitation(token string) (Invitation, error) {
	c.lock()
	defer c.mu.Unlock()
	v, err := c.invitation(token)
	v.TokenHash = ""
	return v, err
}
func (c *Catalog) AcceptInvitation(token, actor, name, password string, local bool) (string, error) {
	c.lock()
	v, err := c.invitation(token)
	c.mu.Unlock()
	if err != nil {
		return "", err
	}
	creating := actor == "" && !v.Reset
	if v.Reset && !local || creating && !local {
		return "", workspace.ErrForbidden
	}
	name = strings.ToLower(strings.TrimSpace(name))
	hash := ""
	if creating || v.Reset {
		if creating && !localName.MatchString(name) {
			return "", ErrLocalInput
		}
		hash, err = passwordHash(password)
		if err != nil {
			return "", err
		}
	}
	c.lock()
	defer c.mu.Unlock()
	v, err = c.invitation(token)
	if err != nil {
		return "", err
	}
	user := actor
	if v.Reset {
		user = v.User
	} else if creating {
		user = "local-" + strings.ToLower(secretToken())
	}
	if !creating {
		u, ok := c.state.Users[user]
		if !ok || u.Disabled || u.Kind == "service" || user == Bootstrap {
			return "", workspace.ErrForbidden
		}
	}
	if err = c.validateInvitationGrants(v.InvitationSpec); err != nil {
		return "", err
	}
	err = c.change(user, "invitation.accept", v.ID, map[string]string{"issuer": v.Issuer}, func() error {
		if v.Reset {
			found := false
			for username, credential := range c.state.Passwords {
				if credential.User == user {
					c.state.Passwords[username] = LocalCredential{User: user, Hash: hash}
					found = true
				}
			}
			if !found {
				return workspace.ErrForbidden
			}
			c.state.AccessEpoch[user]++
		} else {
			if creating {
				if _, exists := c.state.Passwords[name]; exists {
					return workspace.ErrConflict
				}
				c.state.Users[user] = User{ID: user, DisplayName: v.Name}
				c.state.Passwords[name] = LocalCredential{User: user, Hash: hash}
			}
			for _, id := range v.Groups {
				g := c.state.Groups[id]
				if !slices.Contains(g.Members, user) {
					g.Members = append(g.Members, user)
					g.Revision = c.state.ActivitySequence + 1
					c.state.Groups[id] = g
				}
			}
			for id, role := range v.Projects {
				p := c.state.Projects[id]
				if rank(role) > rank(p.Members[user]) {
					if p.Members == nil {
						p.Members = map[string]string{}
					}
					p.Members[user] = role
					c.state.Projects[id] = p
				}
			}
		}
		v.Status = "accepted"
		v.User = user
		v.TokenHash = ""
		c.state.Invitations[v.ID] = v
		return nil
	})
	return user, err
}
