package web

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/messaging"
	"forgetmenote/internal/webcore"
	"forgetmenote/internal/workspace"
)

// Workspace members' direct messages run as the members' service account, so
// they cannot know whose memory to use. A member sends `link` in a direct
// message; the reply holds a short-lived signed link, and signing in to confirm
// it ties that messaging user to the account. The link proves both halves: only
// the messaging user sees the reply, and only the account holder can confirm.
// It carries memory only; what the conversation may read stays the client's
// policy. `unlink` in the direct message or Unlink on the account page ends it.

const messageLinkTTL = 15 * time.Minute

var messageLinkPattern = regexp.MustCompile(`(?i)^\s*(?:<@[A-Z0-9]+>\s*)?(link|unlink)\s*[.!]?\s*$`)

func messageLinkCommand(text string) string {
	if m := messageLinkPattern.FindStringSubmatch(text); m != nil {
		return strings.ToLower(m[1])
	}
	return ""
}

type messageLinkClaim struct {
	Client    string `json:"c"`
	Workspace string `json:"w"`
	User      string `json:"u"`
	Channel   string `json:"ch"`
	Expires   int64  `json:"exp"`
}

func (s *Server) messageLinkToken(d messaging.Address, expires time.Time) string {
	raw, _ := json.Marshal(messageLinkClaim{d.Client, d.Workspace, d.User, d.Channel, expires.Unix()})
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + s.sign("message-link\x00"+payload)
}

// messageLinkAddress returns the direct message a current, untampered token was
// issued in.
func (s *Server) messageLinkAddress(token string) (messaging.Address, error) {
	payload, signature, ok := strings.Cut(token, ".")
	if !ok || subtle.ConstantTimeCompare([]byte(s.sign("message-link\x00"+payload)), []byte(signature)) != 1 {
		return messaging.Address{}, workspace.ErrForbidden
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	var claim messageLinkClaim
	if err != nil || json.Unmarshal(raw, &claim) != nil || !time.Now().Before(time.Unix(claim.Expires, 0)) {
		return messaging.Address{}, webcore.Error{Status: 403, Code: "expired", Message: "This link has expired. Send link in a direct message for a new one."}
	}
	d := messaging.Address{Client: claim.Client, Workspace: claim.Workspace, User: claim.User, Channel: claim.Channel, Direct: true}
	c, _, ok := s.messaging.MessageClient(d.Client)
	if !ok || c.Workspace != d.Workspace || c.Members == nil {
		return messaging.Address{}, workspace.ErrForbidden
	}
	return d, nil
}

func (s *Server) accountLabel(subject string) string {
	account, err := s.catalog.Account(subject)
	if err != nil {
		return subject
	}
	identity := account.Identity()
	switch {
	case identity.Name != "" && identity.Email != "" && identity.Name != identity.Email:
		return identity.Name + " (" + identity.Email + ")"
	case identity.Email != "":
		return identity.Email
	case identity.Name != "":
		return identity.Name
	}
	return subject
}

// linkCommandReply answers `link` or `unlink` in a direct, unshared
// conversation that has no answer yet: only the messaging user sees that
// reply, so only there may it carry a link. ok is false for any other message.
func (s *Server) linkCommandReply(r *automation.Run, c messaging.Client, d messaging.Address) (string, bool, error) {
	command := messageLinkCommand(r.Spec.Instruction)
	if command == "" || !d.Direct || d.Shared || r.Answer != "" {
		return "", false, nil
	}
	text, err := s.messageLinkReply(r, c, d, command)
	return text, true, err
}

// messageLinkReply answers `link` and `unlink` without the model. The reply is
// built when it is sent and never stored, so the run keeps no link token.
func (s *Server) messageLinkReply(r *automation.Run, c messaging.Client, d messaging.Address, command string) (string, error) {
	name := s.catalog.InstanceName()
	if c.Members == nil || r.Spec.Actor != c.Members.Actor {
		return "Your direct messages here already use your " + name + " account and its memory, so there is nothing to link.", nil
	}
	linked := s.catalog.MessageLink(d.Client, c.Workspace, d.User)
	if command == "unlink" {
		if linked == "" {
			return "This account is not linked, so these messages use no one's memory.", nil
		}
		if err := s.catalog.UnlinkMessageUser(linked, d.Client, c.Workspace, d.User); err != nil {
			return "", err
		}
		return "Unlinked. These messages no longer use the memory of " + s.accountLabel(linked) + ".", nil
	}
	if linked != "" {
		return "This account is linked to " + s.accountLabel(linked) + ", so these messages use that memory. Reply `unlink` to remove the link.", nil
	}
	d.Workspace = c.Workspace
	target := s.origin + "/account/link?token=" + s.messageLinkToken(d, time.Now().Add(messageLinkTTL))
	return "To let me use and keep your " + name + " memory in these messages, [sign in and confirm](" + target + ") within 15 minutes. Don't share the link: whoever confirms it links their memory to you here.", nil
}

func (s *Server) messageLinkRoutes(register routeRegistrar) {
	register.global("GET /account/link", func(w http.ResponseWriter, r *http.Request) {
		if err := s.accountUIRequest(r.Context()); err != nil {
			fail(w, err)
			return
		}
		token := r.URL.Query().Get("token")
		d, err := s.messageLinkAddress(token)
		if err != nil {
			fail(w, err)
			return
		}
		linked := s.catalog.MessageLink(d.Client, d.Workspace, d.User)
		status := http.StatusOK
		if linked != "" && linked != subject(r.Context()) {
			status = http.StatusConflict
		}
		var page bytes.Buffer
		if err = s.page.ExecuteTemplate(&page, "message-link.html", map[string]any{"Client": messageClientName(d.Client), "User": d.User, "Workspace": d.Workspace, "Token": token, "Account": s.accountLabel(subject(r.Context())), "Mine": linked == subject(r.Context()), "Other": status == http.StatusConflict}); err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.WriteHeader(status)
		w.Write(page.Bytes())
	})
	register.global("POST /account/link", func(w http.ResponseWriter, r *http.Request) {
		if err := s.accountUIRequest(r.Context()); err != nil {
			fail(w, err)
			return
		}
		d, err := s.messageLinkAddress(r.FormValue("token"))
		if err != nil {
			fail(w, err)
			return
		}
		user := subject(r.Context())
		if err = s.catalog.LinkMessageUser(user, d.Client, d.Workspace, d.User); errors.Is(err, catalog.ErrMessageLinked) {
			err = webcore.Error{Status: 409, Code: "conflict", Message: "This " + messageClientName(d.Client) + " account is linked to another account. Reply unlink to " + s.catalog.InstanceName() + " in that direct message first."}
		}
		if err != nil {
			fail(w, err)
			return
		}
		// Tell the messaging user, so a link they did not confirm is seen at once.
		s.notifyMessageLink(r.Context(), d, user)
		http.Redirect(w, r, "/account/assistant", http.StatusSeeOther)
	})
}

// messageUnlinkRoute removes a person's own message link. It stays with every
// module set: with messaging off nothing uses a link, but a person can still
// withdraw one before messaging runs again.
func (s *Server) messageUnlinkRoute() {
	s.mux.HandleFunc("POST /account/link/remove", func(w http.ResponseWriter, r *http.Request) {
		if err := s.accountUIRequest(r.Context()); err != nil {
			fail(w, err)
			return
		}
		if err := s.catalog.UnlinkMessageUser(subject(r.Context()), r.FormValue("client"), r.FormValue("workspace"), r.FormValue("user")); err != nil {
			fail(w, err)
			return
		}
		http.Redirect(w, r, "/account/assistant", http.StatusSeeOther)
	})
}

func (s *Server) notifyMessageLink(ctx context.Context, d messaging.Address, user string) {
	_, transport, ok := s.messaging.MessageClient(d.Client)
	if !ok {
		return
	}
	text := "This " + messageClientName(d.Client) + " account is now linked to " + s.accountLabel(user) + ", so I use and keep that memory in our direct messages. If you didn't confirm this, reply `unlink`."
	if err := transport.Send(ctx, d, messaging.Reply{Text: text}); err != nil {
		slog.Warn("message link notice failed", "client", d.Client, "error", err)
	}
}

func messageClientName(client string) string {
	if client == "slack" {
		return "Slack"
	}
	return client
}
