//go:build !fmn_omit_slack

package messaging

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/workspace"
)

// slackAnswering is a Slack whose every method answers body.
func slackAnswering(t *testing.T, body string, calls *atomic.Int32) *Slack {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return &Slack{Team: "T1", Endpoint: server.URL + "/chat.postEphemeral", Client: server.Client(), Token: func() (string, error) { return "token", nil }}
}

func TestSlackRefusalsAreReadByCodeNotByMessageText(t *testing.T) {
	var calls atomic.Int32
	// Slack adds an explanation to some refusals; the code still decides.
	explained := `{"ok":false,"error":"%s","response_metadata":{"messages":["[ERROR] explained, missing_scope"]}}`
	for _, c := range []struct {
		code   string
		hidden bool
		scope  bool
	}{
		{"channel_not_found", true, false},
		{"not_in_channel", true, false},
		{"missing_scope", false, true},
		// The explanation names missing_scope, but the code is another.
		{"ratelimited", false, false},
	} {
		s := slackAnswering(t, strings.Replace(explained, "%s", c.code, 1), &calls)
		_, err := s.ChannelMembers(t.Context(), "C123")
		if errors.Is(err, ErrAudienceHidden) != c.hidden || errors.Is(err, ErrAudienceScope) != c.scope {
			t.Fatalf("%s: %v (hidden %v, scope %v)", c.code, err, c.hidden, c.scope)
		}
		if !strings.Contains(err.Error(), "HTTP 200, "+c.code+" (") {
			t.Fatalf("%s: the refusal lost its code or explanation: %v", c.code, err)
		}
	}

	// A reaction that is already there, or already gone, is no failure, even
	// with an explanation attached.
	s := slackAnswering(t, strings.Replace(explained, "%s", "already_reacted", 1), &calls)
	if err := s.Status(t.Context(), Address{Channel: "C123", Message: "1.2"}, "is thinking…"); err != nil {
		t.Fatalf("an existing reaction failed the status: %v", err)
	}
	s = slackAnswering(t, strings.Replace(explained, "%s", "no_reaction", 1), &calls)
	if err := s.Status(t.Context(), Address{Channel: "C123", Message: "1.2"}, ""); err != nil {
		t.Fatalf("a missing reaction failed clearing the status: %v", err)
	}
	if err := s.Status(t.Context(), Address{Channel: "C123", Message: "1.2"}, "is thinking…"); err == nil {
		t.Fatal("no_reaction was ignored while adding a reaction")
	}
}

func TestSlackRequestsAskAllowFirstAndSendNothingWhenRefused(t *testing.T) {
	var calls atomic.Int32
	s := slackAnswering(t, `{"ok":true,"user":{"id":"U1","team_id":"T1"},"ts":"1.2"}`, &calls)
	paused := errors.New("outgoing actions are paused on this instance")
	s.Allow = func() error { return paused }
	d := Address{Workspace: "T1", Channel: "D1", User: "U1", Direct: true}
	hook := strings.TrimSuffix(s.Endpoint, "/chat.postEphemeral") + "/actions/T1/1/hook"
	checks := map[string]error{
		"verify":  s.VerifyMember(t.Context(), d),
		"send":    s.Send(t.Context(), d, Reply{Text: "Hello", Artifacts: []Artifact{{Name: "a.csv", Data: []byte("a")}}}),
		"status":  s.Status(t.Context(), Address{Channel: "C1", Message: "1.2"}, "is thinking…"),
		"respond": s.Respond(t.Context(), hook, "Received."),
		"view":    s.OpenView(t.Context(), "trigger", map[string]any{}),
	}
	_, checks["post"] = s.Post(t.Context(), "C1", Post{Text: "Hello"})
	_, checks["members"] = s.Members(t.Context())
	for name, err := range checks {
		if !errors.Is(err, paused) || !errors.Is(err, ErrNotPosted) {
			t.Fatalf("%s while paused: %v", name, err)
		}
	}
	if _, err := s.download(t.Context(), hook, true, 1); err == nil {
		t.Fatal("a download ran while paused")
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("a paused transport made %d requests", n)
	}

	s.Allow = func() error { return nil }
	if err := s.VerifyMember(t.Context(), d); err != nil || calls.Load() != 1 {
		t.Fatalf("an allowed check: %v after %d requests", err, calls.Load())
	}
}

func TestSlackRemembersMemberChecksBrieflyWithinOneInvocation(t *testing.T) {
	var calls atomic.Int32
	var answer atomic.Value
	answer.Store(`{"ok":true,"user":{"id":"U1","team_id":"T1"}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("user") == "UGUEST" {
			w.Write([]byte(`{"ok":true,"user":{"id":"UGUEST","team_id":"T1","is_restricted":true}}`))
			return
		}
		w.Write([]byte(answer.Load().(string)))
	}))
	defer server.Close()
	now := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	ctx := context.WithValue(t.Context(), verifiedKey{}, &verifiedMembers{now: func() time.Time { return now }})
	if RememberMembers(ctx) != ctx {
		t.Fatal("a nested invocation forgot its answers")
	}
	s := &Slack{Team: "T1", Endpoint: server.URL + "/chat.postEphemeral", Client: server.Client(), Token: func() (string, error) { return "token", nil }}
	member, guest := Address{Workspace: "T1", User: "U1"}, Address{Workspace: "T1", User: "UGUEST"}
	check := func(ctx context.Context, d Address, want error, asked int32) {
		t.Helper()
		if err := s.VerifyMember(ctx, d); !errors.Is(err, want) || calls.Load() != asked {
			t.Fatalf("%s: %v after %d requests; want %v after %d", d.User, err, calls.Load(), want, asked)
		}
	}
	check(ctx, member, nil, 1)
	check(ctx, member, nil, 1)
	// A refusal is remembered too.
	check(ctx, guest, workspace.ErrForbidden, 2)
	check(ctx, guest, workspace.ErrForbidden, 2)
	// Another workspace's member of the same ID is another member.
	other := *s
	other.Team = "T2"
	if err := other.VerifyMember(ctx, Address{Workspace: "T2", User: "U1"}); !errors.Is(err, workspace.ErrForbidden) || calls.Load() != 3 {
		t.Fatalf("another workspace's member: %v after %d requests", err, calls.Load())
	}
	now = now.Add(memberAnswerTTL)
	check(ctx, member, nil, 4)

	// A failure to ask is never remembered.
	answer.Store(`{"ok":false,"error":"internal_error"}`)
	now = now.Add(memberAnswerTTL)
	if err := s.VerifyMember(ctx, member); err == nil || errors.Is(err, workspace.ErrForbidden) {
		t.Fatalf("a failed check: %v", err)
	}
	answer.Store(`{"ok":true,"user":{"id":"U1","team_id":"T1"}}`)
	check(ctx, member, nil, 6)
	// Every other invocation asks again.
	check(t.Context(), member, nil, 7)
	check(RememberMembers(t.Context()), member, nil, 8)
}

func TestSlackReplyRefusedAfterItsFirstMessageIsNoLongerUnposted(t *testing.T) {
	long := Reply{Text: strings.Repeat("a", messageLimit) + "b"}
	for _, c := range []struct {
		name      string
		refuse    int32 // the post Slack refuses, counting from 1
		reply     Reply
		notPosted bool
	}{
		{"first of two refused", 1, long, true},
		{"second of two refused", 2, long, false},
		{"confirmation refused", 2, Reply{Text: "Proposed.", Confirmations: []Confirmation{{Code: "ABC123", Label: "the write"}}}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if posts.Add(1) == c.refuse {
					w.Write([]byte(`{"ok":false,"error":"msg_too_long"}`))
					return
				}
				w.Write([]byte(`{"ok":true,"ts":"1.2"}`))
			}))
			defer server.Close()
			s := &Slack{Team: "T1", Endpoint: server.URL + "/chat.postEphemeral", Client: server.Client(), Token: func() (string, error) { return "token", nil }}
			err := s.Send(t.Context(), Address{Workspace: "T1", Channel: "D1", User: "U1", Direct: true}, c.reply)
			if err == nil || errors.Is(err, ErrNotPosted) != c.notPosted || !strings.Contains(err.Error(), "msg_too_long") {
				t.Fatalf("after %d posts: %v; not posted should be %v", posts.Load(), err, c.notPosted)
			}
		})
	}
}
