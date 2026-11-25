// Package messaging defines the boundary between chat clients and the shared assistant.
// Transports own network protocols; notebook access and execution stay in the server.
package messaging

import (
	"context"
	"errors"
	"regexp"
)

type Binding struct {
	Project            string   `json:"project"`
	ReadProjects       []string `json:"readProjects,omitempty"`
	AIConnection       string   `json:"aiConnection"`
	Model              string   `json:"model"`
	Connections        []string `json:"connections,omitempty"`
	ExternalProcessing bool     `json:"externalProcessing"`
	Shared             bool     `json:"shared,omitempty"`
	AudienceApproved   bool     `json:"audienceApproved,omitempty"`
	Actors             []string `json:"actors,omitempty"`
	ThreadHistory      bool     `json:"threadHistory,omitempty"`
	Attachments        bool     `json:"attachments,omitempty"`
}

type Broadcast struct {
	Project          string   `json:"project"`
	Actors           []string `json:"actors"`
	Connections      []string `json:"connections"`
	AudienceApproved bool     `json:"audienceApproved"`
}

// Client contains policy only. Transport credentials never enter this structure.
type Client struct {
	Members           *MemberAccess        `json:"members,omitempty"`
	Workspace         string               `json:"workspace"`
	Disabled          bool                 `json:"disabled,omitempty"`
	Users             map[string]string    `json:"users"`
	Channels          map[string]Binding   `json:"channels"`
	DirectMessages    map[string]Binding   `json:"directMessages,omitempty"`
	KnowledgeChannels map[string]string    `json:"knowledgeChannels,omitempty"`
	Broadcasts        map[string]Broadcast `json:"broadcasts,omitempty"`
}

// MemberAccess explicitly delegates a read scope to verified workspace members.
// The actor is a service account; private transcripts remain isolated by member.
type MemberAccess struct {
	Actor   string  `json:"actor"`
	Binding Binding `json:"binding"`
	// PairByEmail lists the Google Workspace domains whose verified sign-ins may
	// pair members to accounts by email (the message-pairing automation).
	PairByEmail []string `json:"pairByEmail,omitempty"`
}

// Member is an active, human, non-guest workspace member with the address the
// provider confirmed for them, or none.
type Member struct {
	User  string `json:"user"`
	Email string `json:"-"`
	// TimeZone is the member's IANA time zone, and Status and StatusEmoji
	// their current custom status, when the workspace reports them.
	TimeZone    string `json:"-"`
	Status      string `json:"-"`
	StatusEmoji string `json:"-"`
	// Name is the member's display name, or else their full name.
	Name string `json:"-"`
}

// MemberLister is implemented by transports that can list a workspace's members.
type MemberLister interface {
	Members(context.Context) ([]Member, error)
}

// AudienceLister is implemented by transports that can list the members of a
// channel or a user group, by member ID and in the provider's order. The
// lists may include guests, apps and other workspaces' members.
type AudienceLister interface {
	ChannelMembers(ctx context.Context, channel string) ([]string, error)
	GroupMembers(ctx context.Context, group string) ([]string, error)
}

// ErrAudienceHidden reports a channel or group the transport cannot see, and
// ErrAudienceScope one its app lacks the permission to list.
var (
	ErrAudienceHidden = errors.New("channel or group not visible")
	ErrAudienceScope  = errors.New("listing members needs another app permission")
)

// MemberVerifier is implemented by transports that can authenticate membership
// independently of the event's app-installation workspace.
type MemberVerifier interface {
	VerifyMember(context.Context, Address) error
}

type StatusTransport interface {
	Status(context.Context, Address, string) error
}

type File struct {
	ID string `json:"id"`
}

type Address struct {
	Client    string `json:"client,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Channel   string `json:"channel"`
	Thread    string `json:"thread"`
	User      string `json:"user,omitempty"`
	Message   string `json:"message,omitempty"`
	Direct    bool   `json:"direct,omitempty"`
	Shared    bool   `json:"shared,omitempty"`
	Policy    string `json:"policy,omitempty"`
	Files     []File `json:"files,omitempty"`
	// Existing queued Slack jobs use this field. New clients use User.
	SlackUser string `json:"slackUser,omitempty"`
}

type Event struct {
	ID, Text string
	Address  Address
}
type ContextMessage struct{ ID, User, Text string }
type Document struct{ Name, URL, Text, Hash, Warning string }
type Context struct {
	Messages  []ContextMessage
	Documents []Document
}
type Artifact struct {
	Name    string `json:"name"`
	MIME    string `json:"mime"`
	Data    []byte `json:"data"`
	Receipt string `json:"receipt,omitempty"`
	Caption string `json:"caption,omitempty"`
}
type Reply struct {
	Text      string
	Artifacts []Artifact
	// Confirmations are changes proposed in Text. A transport that can offer
	// buttons does so; pressing one is the same as replying with the command.
	Confirmations []Confirmation
	// Buttons go below a direct message's text, where a transport can show them.
	Buttons []Button
	// Sections follow a direct message's text, each a line with the buttons
	// that act on it, before any Buttons.
	Sections []Section
}

// Button is an action offered with a direct message. Pressing it returns
// Action and Value to the client's interaction endpoint, where the pressing
// user's permission is checked again: neither authorizes anything.
type Button struct {
	Action string
	Label  string
	Value  string
	Style  string // primary, danger or empty
}

// Section is a line of a direct message, such as one item of a list, with the
// buttons that act on it.
type Section struct {
	Text    string
	Buttons []Button
}

// Confirmation names one proposed change by its one-time code and a short label.
type Confirmation struct {
	Code  string
	Label string
}

// ButtonValue is what a confirmation button carries back: the code, and the
// thread of the conversation that proposed it, which the click itself does not
// identify. Authorization never depends on it: the clicking user must be the
// requester of that code in that conversation.
type ButtonValue struct {
	Code   string `json:"code"`
	Thread string `json:"thread"`
}

// Button actions and the reply each one stands for.
const (
	ConfirmAction = "confirm_write"
	CancelAction  = "cancel_write"
)

type Transport interface {
	Context(context.Context, Address, Binding) (Context, error)
	Send(context.Context, Address, Reply) error
}

var workspaceDomain = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:@-]{0,127}$`)

func ValidID(id string) bool { return identifier.MatchString(id) }
func (c Client) Validate() error {
	if c.Members != nil {
		b := c.Members.Binding
		if !ValidID(c.Members.Actor) || b.Shared && (len(b.Actors) != 1 || b.Actors[0] != c.Members.Actor) || !b.Shared && len(b.Actors) > 0 {
			return errors.New("workspace members require a service actor; shared channels must use that actor alone")
		}
		if err := c.Members.Binding.Validate(); err != nil {
			return err
		}
		if len(c.Members.PairByEmail) > 16 {
			return errors.New("pairByEmail lists at most 16 domains")
		}
		for _, domain := range c.Members.PairByEmail {
			if len(domain) > 253 || !workspaceDomain.MatchString(domain) {
				return errors.New("pairByEmail must list lowercase Google Workspace domains, such as example.com")
			}
		}
	}
	if !ValidID(c.Workspace) || len(c.Users) > 1000 || len(c.Channels)+len(c.Broadcasts)+len(c.KnowledgeChannels) > 128 || len(c.DirectMessages) > 1000 {
		return errors.New("invalid chat client configuration")
	}
	for id, user := range c.Users {
		if !ValidID(id) || user == "" || len(user) > 128 {
			return errors.New("invalid chat account mapping")
		}
	}
	for id, b := range c.Channels {
		if !ValidID(id) {
			return errors.New("invalid channel ID")
		}
		if err := b.Validate(); err != nil {
			return err
		}
	}
	for id, b := range c.DirectMessages {
		if c.Users[id] == "" || b.Shared {
			return errors.New("direct messages require a linked account and private replies")
		}
		if err := b.Validate(); err != nil {
			return err
		}
	}
	for id, p := range c.KnowledgeChannels {
		if !ValidID(id) || !ValidID(p) {
			return errors.New("invalid knowledge channel")
		}
	}
	for id, b := range c.Broadcasts {
		if !ValidID(id) || !b.AudienceApproved || !ValidID(b.Project) || len(b.Actors) == 0 || len(b.Actors) > 128 {
			return errors.New("broadcasts require an approved audience and notebook")
		}
	}
	return nil
}

// Resolve preserves explicit user/channel bindings. Workspace channel sharing
// requires explicit audience approval and never changes a DM's private scope.
func (c Client) Resolve(d Address) (Binding, string, bool, bool) {
	b, ok := c.Channels[d.Channel]
	if d.Direct {
		b, ok = c.DirectMessages[d.User]
	}
	if ok && c.Users[d.User] != "" {
		return b, c.Users[d.User], false, true
	}
	if c.Members != nil {
		b = c.Members.Binding
		if d.Direct {
			b.Shared, b.AudienceApproved, b.Actors = false, false, nil
		}
		return b, c.Members.Actor, true, true
	}
	return Binding{}, "", false, false
}
func (b Binding) Validate() error {
	if !ValidID(b.Project) || !b.ExternalProcessing || len(b.ReadProjects) > 32 || len(b.Connections) > 64 || len(b.Actors) > 128 {
		return errors.New("chat destinations require a notebook, bounded scope and processing consent")
	}
	for _, id := range append(append([]string{}, b.ReadProjects...), b.Connections...) {
		if !ValidID(id) {
			return errors.New("invalid chat destination scope")
		}
	}
	if b.Shared && (!b.AudienceApproved || len(b.Actors) == 0) {
		return errors.New("shared replies require an approved audience")
	}
	return nil
}
