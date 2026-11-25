package catalog

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"forgetmenote/internal/messaging"
)

type MessagePairingConflict struct {
	User     string   `json:"user"`
	Accounts []string `json:"accounts,omitempty"`
	Reason   string   `json:"reason"`
}

// MessagePairing reports one pairing run. It names members and accounts by ID
// and never carries an address.
type MessagePairing struct {
	Members   int                      `json:"members"`
	Paired    []MessageLink            `json:"paired"`
	Unpaired  []MessageLink            `json:"unpaired"`
	Current   int                      `json:"current"`
	Declined  int                      `json:"declined"`
	Conflicts []MessagePairingConflict `json:"conflicts"`
}

// PairMessageUsersByEmail links each workspace member to the one active account
// whose latest sign-in verified the same address under one of hostedDomains,
// and removes the automatic links that no longer match. A link someone made,
// and a pair someone unlinked, are never changed by it. Members the client's
// configuration maps itself are skipped, and ambiguity on either side is
// reported, never guessed. members must be the workspace's complete list: a
// member missing from it loses an automatic link. A dry run reports what a run
// would change.
func (c *Catalog) PairMessageUsersByEmail(actor, client, workspaceID string, hostedDomains []string, members []messaging.Member, mapped map[string]bool, dryRun bool) (MessagePairing, error) {
	report := MessagePairing{Members: len(members), Paired: []MessageLink{}, Unpaired: []MessageLink{}, Conflicts: []MessagePairingConflict{}}
	if !validMessageLink(client, workspaceID, "member") || len(members) > 100000 {
		return report, errors.New("invalid messaging workspace")
	}
	domains := map[string]bool{}
	for _, d := range hostedDomains {
		d = strings.ToLower(strings.TrimSpace(d))
		if !validHostedDomain(d) {
			return report, errors.New("invalid hosted domain for email pairing")
		}
		domains[d] = true
	}
	if len(domains) == 0 {
		return report, errors.New("email pairing needs at least one hosted domain")
	}
	members = slices.Clone(members)
	slices.SortFunc(members, func(a, b messaging.Member) int { return strings.Compare(a.User, b.User) })
	address := map[string]string{}
	sharing := map[string]int{}
	for _, m := range members {
		if email := strings.ToLower(validEmail(m.Email)); email != "" && validMessageLink(client, workspaceID, m.User) {
			address[m.User] = email
			sharing[email]++
		}
	}
	if len(address) == 0 {
		return report, errors.New("the member list holds no email addresses; check that the messaging app may read them")
	}

	c.lock()
	defer c.mu.Unlock()
	if c.version < 0 {
		return report, ErrStorageUnavailable
	}
	verified := map[string][]string{}
	for _, v := range c.state.OIDC {
		u, ok := c.state.Users[v.User]
		if v.Email != "" && domains[v.HostedDomain] && ok && !u.Disabled && u.Kind != "service" && !slices.Contains(verified[v.Email], v.User) {
			verified[v.Email] = append(verified[v.Email], v.User)
		}
	}
	links := map[string]MessageLink{}
	linkedMembers := map[string][]string{}
	for _, l := range c.state.MessageLinks {
		if l.Client == client && l.Workspace == workspaceID {
			links[l.User] = l
			linkedMembers[l.Account] = append(linkedMembers[l.Account], l.User)
		}
	}
	conflict := func(user, reason string, accounts ...string) {
		slices.Sort(accounts)
		report.Conflicts = append(report.Conflicts, MessagePairingConflict{User: user, Accounts: accounts, Reason: reason})
	}

	match := map[string]string{}
	matched := map[string]int{}
	for _, m := range members {
		email := address[m.User]
		accounts := verified[email]
		switch {
		case email == "" || mapped[m.User] || len(accounts) == 0:
		case sharing[email] > 1:
			conflict(m.User, "several members share this address")
		case len(accounts) > 1:
			conflict(m.User, "several accounts verified this address", slices.Clone(accounts)...)
		default:
			match[m.User] = accounts[0]
			matched[accounts[0]]++
		}
	}
	want := map[string]string{}
	for _, m := range members {
		account, ok := match[m.User]
		if !ok {
			continue
		}
		if matched[account] > 1 {
			conflict(m.User, "the account verified the addresses of several members", account)
			continue
		}
		if l, ok := links[m.User]; ok && l.Source != "email" {
			if l.Account != account {
				conflict(m.User, "the member is linked to another account", l.Account)
			}
			continue
		}
		if slices.ContainsFunc(linkedMembers[account], func(u string) bool { return u != m.User && links[u].Source != "email" }) {
			conflict(m.User, "the account is linked to another member", account)
			continue
		}
		if slices.ContainsFunc(c.state.MessageDeclines, func(d MessageLinkDecline) bool {
			return d.Client == client && d.Workspace == workspaceID && d.User == m.User && d.Account == account
		}) {
			report.Declined++
			continue
		}
		want[m.User] = account
	}

	next := make([]MessageLink, 0, len(c.state.MessageLinks)+len(want))
	for _, l := range c.state.MessageLinks {
		if l.Client == client && l.Workspace == workspaceID && l.Source == "email" && want[l.User] != l.Account {
			report.Unpaired = append(report.Unpaired, l)
			continue
		}
		next = append(next, l)
	}
	now := time.Now().UTC()
	for _, m := range members {
		account, ok := want[m.User]
		if !ok || links[m.User].Source == "email" && links[m.User].Account == account {
			continue
		}
		l := MessageLink{Client: client, Workspace: workspaceID, User: m.User, Account: account, Linked: now, Source: "email"}
		report.Paired = append(report.Paired, l)
		next = append(next, l)
	}
	report.Current = len(want)
	if dryRun || len(report.Paired)+len(report.Unpaired) == 0 {
		return report, nil
	}
	details := map[string]string{"client": client, "workspace": workspaceID, "paired": strconv.Itoa(len(report.Paired)), "unpaired": strconv.Itoa(len(report.Unpaired))}
	return report, c.change(actor, "account.message.email_pairing", workspaceID, details, func() error {
		c.state.MessageLinks = next
		return nil
	})
}

// MessagePairingAffected reports whether a pairing run could change anything
// for one member, so that a member event, most often a status change, starts a
// run only when it matters. active reports whether the member still counts as
// an active, human, non-guest member.
func (c *Catalog) MessagePairingAffected(client, workspaceID string, hostedDomains []string, m messaging.Member, active bool) bool {
	domains := map[string]bool{}
	for _, d := range hostedDomains {
		domains[strings.ToLower(strings.TrimSpace(d))] = true
	}
	email := strings.ToLower(validEmail(m.Email))
	c.lock()
	defer c.mu.Unlock()
	i := slices.IndexFunc(c.state.MessageLinks, func(l MessageLink) bool {
		return l.Client == client && l.Workspace == workspaceID && l.User == m.User
	})
	if i >= 0 && c.state.MessageLinks[i].Source != "email" {
		return false
	}
	if !active || email == "" {
		return i >= 0
	}
	verified := []string{}
	for _, v := range c.state.OIDC {
		u, ok := c.state.Users[v.User]
		if v.Email == email && domains[v.HostedDomain] && ok && !u.Disabled && u.Kind != "service" && !slices.Contains(verified, v.User) {
			verified = append(verified, v.User)
		}
	}
	if i >= 0 {
		return len(verified) != 1 || verified[0] != c.state.MessageLinks[i].Account
	}
	return len(verified) > 0
}
