package catalog

import (
	"errors"
	"slices"
	"time"

	"forgetmenote/internal/messaging"
	"forgetmenote/internal/workspace"
)

// MessageLink records that one person proved they hold both a messaging user,
// by asking for a link in a direct message, and an account, by signing in to
// confirm it. It lets that messaging user's direct messages use the account's
// memory and nothing else: access and notebooks still come from the chat
// client's policy, which administrators own. An older release drops links when
// it rewrites the catalog; people then link again.
type MessageLink struct {
	Client    string    `json:"client"`
	Workspace string    `json:"workspace"`
	User      string    `json:"user"`
	Account   string    `json:"account"`
	Linked    time.Time `json:"linked"`
	Source    string    `json:"source,omitempty"`
}

// MessageLinkDecline records that a person unlinked a messaging user from an
// account. Automatic pairing never links that pair again; only the person
// linking it themselves clears it.
type MessageLinkDecline struct {
	Client    string    `json:"client"`
	Workspace string    `json:"workspace"`
	User      string    `json:"user"`
	Account   string    `json:"account"`
	Declined  time.Time `json:"declined"`
}

// ErrMessageLinked means the messaging user is linked to another account. It
// stays linked until that link is removed.
var ErrMessageLinked = errors.New("this messaging account is already linked to another account")

func validMessageLink(client, workspaceID, user string) bool {
	return messaging.ValidID(client) && messaging.ValidID(workspaceID) && messaging.ValidID(user)
}

// LinkMessageUser links a messaging user to the signed-in subject and clears an
// earlier unlink of that messaging user. Linking an automatic pairing makes it
// the subject's own link; linking it again changes nothing.
func (c *Catalog) LinkMessageUser(subject, client, workspaceID, user string) error {
	c.lock()
	defer c.mu.Unlock()
	if !validMessageLink(client, workspaceID, user) || !ValidUser(subject) || c.state.Users[subject].Disabled || c.state.Users[subject].Kind == "service" {
		return workspace.ErrForbidden
	}
	for i, link := range c.state.MessageLinks {
		if link.Client == client && link.Workspace == workspaceID && link.User == user {
			if link.Account != subject {
				return ErrMessageLinked
			}
			if link.Source == "" {
				return nil
			}
			// Confirming an automatic pairing makes it the person's own link.
			return c.change(subject, "account.message.link", subject, map[string]string{"client": client, "workspace": workspaceID}, func() error {
				c.state.MessageLinks[i].Source = ""
				return nil
			})
		}
	}
	return c.change(subject, "account.message.link", subject, map[string]string{"client": client, "workspace": workspaceID}, func() error {
		c.state.MessageLinks = append(c.state.MessageLinks, MessageLink{Client: client, Workspace: workspaceID, User: user, Account: subject, Linked: time.Now().UTC()})
		c.state.MessageDeclines = slices.DeleteFunc(c.state.MessageDeclines, func(d MessageLinkDecline) bool {
			return d.Client == client && d.Workspace == workspaceID && d.User == user
		})
		return nil
	})
}

// UnlinkMessageUser removes the subject's own link, however it was made, and
// keeps automatic pairing from linking it again. Removing a link that is
// already gone changes nothing.
func (c *Catalog) UnlinkMessageUser(subject, client, workspaceID, user string) error {
	c.lock()
	defer c.mu.Unlock()
	i := slices.IndexFunc(c.state.MessageLinks, func(link MessageLink) bool {
		return link.Client == client && link.Workspace == workspaceID && link.User == user
	})
	if i < 0 {
		return nil
	}
	if c.state.MessageLinks[i].Account != subject {
		return workspace.ErrForbidden
	}
	return c.change(subject, "account.message.unlink", subject, map[string]string{"client": client, "workspace": workspaceID}, func() error {
		c.state.MessageLinks = slices.Delete(c.state.MessageLinks, i, i+1)
		declines := slices.DeleteFunc(c.state.MessageDeclines, func(d MessageLinkDecline) bool {
			return d.Client == client && d.Workspace == workspaceID && d.User == user
		})
		c.state.MessageDeclines = append(declines, MessageLinkDecline{Client: client, Workspace: workspaceID, User: user, Account: subject, Declined: time.Now().UTC()})
		return nil
	})
}

// MessageLink returns the account a messaging user linked, if any. Callers
// still check that the account may act.
func (c *Catalog) MessageLink(client, workspaceID, user string) string {
	c.lock()
	defer c.mu.Unlock()
	for _, link := range c.state.MessageLinks {
		if link.Client == client && link.Workspace == workspaceID && link.User == user {
			return link.Account
		}
	}
	return ""
}

// MessageLinks lists the subject's own links.
func (c *Catalog) MessageLinks(subject string) []MessageLink {
	c.lock()
	defer c.mu.Unlock()
	out := []MessageLink{}
	for _, link := range c.state.MessageLinks {
		if link.Account == subject {
			out = append(out, link)
		}
	}
	return out
}
