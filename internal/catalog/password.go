package catalog

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"forgetmenote/internal/workspace"
)

// Local credentials live in the private catalog, never a project's Git history.
// Login names are distinct from user IDs so they cannot claim proxy/OIDC accounts.
type LocalCredential struct {
	User string `json:"user"`
	Hash string `json:"hash"`
}

var localName = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

// productionPasswordIterations is the PBKDF2 cost of every stored password.
const productionPasswordIterations = 600000

// passwordIterations is the cost of new hashes. Only a test binary lowers it,
// through UseTestPasswordCost. A stored hash verifies only at the production
// cost or at this one, so a server never accepts a cheaper hash.
var passwordIterations = productionPasswordIterations

// UseTestPasswordCost makes new local password hashes cheap for the rest of a
// test binary. Under the race detector one production hash takes about a
// second. Call it from TestMain; it panics in any other binary.
func UseTestPasswordCost() {
	if !testing.Testing() {
		panic("catalog: UseTestPasswordCost outside a test binary")
	}
	passwordIterations = 1000
}

var ErrLocalInput = errors.New("invalid local account input")

func passwordHash(password string) (string, error) {
	if len(password) < 12 || len(password) > 256 {
		return "", fmt.Errorf("%w: use a password of 12–256 bytes", ErrLocalInput)
	}
	salt := make([]byte, 16)
	rand.Read(salt)
	iterations := passwordIterations
	key, err := pbkdf2.Key(sha256.New, password, salt, iterations, 32)
	if err != nil {
		return "", err
	}
	return "pbkdf2-sha256:" + strconv.Itoa(iterations) + ":" + hex.EncodeToString(salt) + ":" + hex.EncodeToString(key), nil
}
func passwordMatches(hash, password string) bool {
	if len(password) > 256 {
		return false
	}
	parts := strings.Split(hash, ":")
	salt := make([]byte, 16)
	expected := make([]byte, 32)
	valid := false
	iterations := passwordIterations
	if len(parts) == 4 && parts[0] == "pbkdf2-sha256" && (parts[1] == strconv.Itoa(productionPasswordIterations) || parts[1] == strconv.Itoa(iterations)) {
		a, e1 := hex.DecodeString(parts[2])
		b, e2 := hex.DecodeString(parts[3])
		if e1 == nil && e2 == nil && len(a) == 16 && len(b) == 32 {
			salt = a
			expected = b
			iterations, _ = strconv.Atoi(parts[1])
			valid = true
		}
	}
	// Unknown users take the same derivation path as an incorrect password.
	key, err := pbkdf2.Key(sha256.New, password, salt, iterations, 32)
	return err == nil && subtle.ConstantTimeCompare(key, expected) == 1 && valid
}

// InitialSetupAvailable uses existing user records, including disabled users, so
// later demotion or disable decisions never reopen first-run setup.
func (c *Catalog) InitialSetupAvailable() bool {
	c.lock()
	defer c.mu.Unlock()
	return len(c.state.Users) == 0
}
func (c *Catalog) CreateInitialAdmin(actor, name, password string) (string, error) {
	if actor != Bootstrap || !c.InitialSetupAvailable() {
		return "", workspace.ErrForbidden
	}
	return c.createLocalUser(actor, name, password, true)
}
func (c *Catalog) CreateLocalUser(actor, name, password string) (string, error) {
	return c.createLocalUser(actor, name, password, false)
}
func (c *Catalog) createLocalUser(actor, name, password string, initialAdmin bool) (string, error) {
	if !c.IsAdmin(actor) {
		return "", workspace.ErrForbidden
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if !localName.MatchString(name) {
		return "", fmt.Errorf("%w: use a username starting with a letter, followed by letters, numbers, dots, underscores or hyphens (64 characters maximum)", ErrLocalInput)
	}
	hash, err := passwordHash(password)
	if err != nil {
		return "", err
	}
	raw := make([]byte, 16)
	rand.Read(raw)
	id := "local-" + hex.EncodeToString(raw)
	c.lock()
	defer c.mu.Unlock()
	if !c.admin(actor) || initialAdmin && (actor != Bootstrap || len(c.state.Users) != 0) {
		return "", workspace.ErrForbidden
	}
	if _, ok := c.state.Passwords[name]; ok {
		return "", workspace.ErrConflict
	}
	if err = c.change(actor, "account.create", id, nil, func() error {
		c.state.Passwords[name] = LocalCredential{id, hash}
		c.state.Users[id] = User{ID: id, Admin: initialAdmin}
		return nil
	}); err != nil {
		return "", err
	}
	return id, nil
}
func (c *Catalog) AuthenticateLocal(name, password string) (string, uint64, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	c.lock()
	credential := c.state.Passwords[name]
	epoch := c.state.AccessEpoch[credential.User]
	c.mu.Unlock()
	matches := passwordMatches(credential.Hash, password)
	c.lock()
	defer c.mu.Unlock()
	if c.version < 0 || !matches || credential.User == "" || c.state.Passwords[name] != credential || c.state.Users[credential.User].Disabled || c.state.Users[credential.User].Kind == "service" || c.state.AccessEpoch[credential.User] != epoch {
		return "", 0, workspace.ErrForbidden
	}
	return credential.User, epoch, nil
}
func (c *Catalog) LocalUsername(id string) string {
	c.lock()
	defer c.mu.Unlock()
	for name, cred := range c.state.Passwords {
		if cred.User == id {
			return name
		}
	}
	return ""
}

// Self-service changes require the old password; an Admin can reset other users.
// Both operations revoke all of the target's existing browser and MCP sessions.
func (c *Catalog) SetLocalPassword(actor, id, current, next string) error {
	name := c.LocalUsername(id)
	if name == "" {
		return workspace.ErrForbidden
	}
	var epoch uint64
	if actor == id {
		_, v, err := c.AuthenticateLocal(name, current)
		if err != nil {
			return err
		}
		epoch = v
	} else if !c.IsAdmin(actor) {
		return workspace.ErrForbidden
	}
	hash, err := passwordHash(next)
	if err != nil {
		return err
	}
	c.lock()
	defer c.mu.Unlock()
	if c.state.Users[id].Disabled || actor != id && !c.admin(actor) || actor == id && c.state.AccessEpoch[id] != epoch {
		return workspace.ErrForbidden
	}
	return c.change(actor, "password.change", id, nil, func() error {
		c.state.Passwords[name] = LocalCredential{id, hash}
		c.state.AccessEpoch[id]++
		return nil
	})
}
