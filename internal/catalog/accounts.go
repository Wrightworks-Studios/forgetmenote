package catalog

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"forgetmenote/internal/attribution"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/workspace"
)

type GitHubAccount struct {
	ID          int64     `json:"id"`
	Login       string    `json:"login"`
	Created     time.Time `json:"created"`
	Email       string    `json:"email,omitempty"`
	Attribution bool      `json:"attribution"`
}
type Account struct {
	ID         string         `json:"id"`
	GitHub     *GitHubAccount `json:"github,omitempty"`
	Generation uint64         `json:"generation"`
}

func (a Account) LocalIdentity() attribution.Identity {
	return attribution.Identity{ID: a.ID, Name: "User " + a.ID[:12], Email: a.ID + "@users.gitnotes.invalid"}
}
func (a Account) Identity() attribution.Identity {
	identity := a.LocalIdentity()
	if a.GitHub != nil && a.GitHub.Attribution && a.GitHub.validEmail(a.GitHub.Email) {
		identity.Name = a.GitHub.Login
		identity.Email = a.GitHub.Email
	}
	return identity
}

// Callers authenticate the subject before resolving its account. Register a
// missing directory entry with no grants, including for legacy proxy accounts.
// Account IDs remain independent of login names and GitHub links.
func (c *Catalog) Account(subject string) (Account, error) {
	c.lock()
	defer c.mu.Unlock()
	a, err := c.account(subject)
	// A first request can reach this gateway just after another one created
	// the account. The stored document then wins: reload and use it.
	if errors.Is(err, pg.ErrConflict) && !errors.Is(err, errCommitUnknown) {
		c.refresh()
		a, err = c.account(subject)
	}
	return a, err
}

// account returns subject's account, creating it on first use.
func (c *Catalog) account(subject string) (Account, error) {
	if c.version < 0 || subject != Bootstrap && (!ValidUser(subject) || c.state.Users[subject].Disabled) {
		return Account{}, workspace.ErrForbidden
	}
	a, hasAccount := c.state.Accounts[subject]
	_, hasUser := c.state.Users[subject]
	if hasAccount && (hasUser || subject == Bootstrap) {
		return cloneAccount(a), nil
	}
	if !hasAccount {
		raw := make([]byte, 16)
		rand.Read(raw)
		a = Account{ID: hex.EncodeToString(raw)}
		c.state.Accounts[subject] = a
	}
	if !hasUser && subject != Bootstrap {
		c.state.Users[subject] = User{ID: subject}
	}
	if err := c.persist(); err != nil {
		if !hasAccount {
			delete(c.state.Accounts, subject)
		}
		if !hasUser {
			delete(c.state.Users, subject)
		}
		return Account{}, err
	}
	return cloneAccount(a), nil
}
func cloneAccount(a Account) Account {
	if a.GitHub != nil {
		g := *a.GitHub
		a.GitHub = &g
	}
	return a
}

var githubLogin = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,99}$`)

func (g GitHubAccount) validEmail(email string) bool {
	modern := fmt.Sprintf("%d+%s@users.noreply.github.com", g.ID, g.Login)
	legacy := g.Login + "@users.noreply.github.com"
	return email == modern || !g.Created.IsZero() && g.Created.Before(time.Date(2017, 7, 18, 0, 0, 0, 0, time.UTC)) && email == legacy
}

// LinkGitHub is called only after the server has verified /user using the OAuth token.
// Generation makes disconnect win over an authorization already in flight.
func (c *Catalog) LinkGitHub(subject string, g GitHubAccount, generation uint64) error {
	c.lock()
	defer c.mu.Unlock()
	a, ok := c.state.Accounts[subject]
	if !ok || c.state.Users[subject].Disabled {
		return workspace.ErrForbidden
	}
	if a.Generation != generation {
		return workspace.ErrConflict
	}
	if g.ID <= 0 || !githubLogin.MatchString(g.Login) || g.Created.IsZero() {
		return errors.New("invalid GitHub account")
	}
	for owner, account := range c.state.Accounts {
		if owner != subject && account.GitHub != nil && account.GitHub.ID == g.ID {
			return errors.New("this GitHub account is already linked to another user")
		}
	}
	if a.GitHub != nil && a.GitHub.ID != g.ID {
		return errors.New("disconnect the current GitHub account before connecting another")
	}
	// Reauthorization refreshes the verified login but requires a fresh attribution opt-in.
	g.Email = ""
	g.Attribution = false
	next := a
	next.GitHub = &g
	next.Generation++
	return c.saveAccount(subject, a, next)
}
func (c *Catalog) GitHubAttribution(subject, email string, enabled bool) error {
	c.lock()
	defer c.mu.Unlock()
	a, ok := c.state.Accounts[subject]
	if !ok || a.GitHub == nil || c.state.Users[subject].Disabled {
		return workspace.ErrForbidden
	}
	next := cloneAccount(a)
	email = strings.TrimSpace(email)
	if enabled && !next.GitHub.validEmail(email) {
		return errors.New("use the noreply address from the connected GitHub account's email settings")
	}
	next.GitHub.Attribution = enabled
	if enabled {
		next.GitHub.Email = email
	}
	return c.saveAccount(subject, a, next)
}
func (c *Catalog) DisconnectGitHub(subject string) error {
	c.lock()
	defer c.mu.Unlock()
	a, ok := c.state.Accounts[subject]
	if !ok || c.state.Users[subject].Disabled {
		return workspace.ErrForbidden
	}
	next := a
	next.GitHub = nil
	next.Generation++
	return c.saveAccount(subject, a, next)
}
func (c *Catalog) saveAccount(subject string, old, next Account) error {
	c.state.Accounts[subject] = next
	if err := c.persist(); err != nil {
		c.state.Accounts[subject] = old
		return err
	}
	return nil
}
