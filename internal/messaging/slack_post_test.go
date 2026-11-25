//go:build !fmn_omit_slack

package messaging

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSlackPostSeparatesRefusalsFromUnknownOutcomes(t *testing.T) {
	for _, c := range []struct {
		name      string
		status    int
		body      string
		notPosted bool
	}{
		{"refused", 200, `{"ok":false,"error":"channel_not_found"}`, true},
		{"rate limited", 429, `{"ok":false,"error":"ratelimited"}`, true},
		{"gateway error", 503, `{"ok":false,"error":"service_unavailable"}`, false},
		{"unreadable", 200, `<html>`, false},
		{"missing ID", 200, `{"ok":true}`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				w.Write([]byte(c.body))
			}))
			defer server.Close()
			s := &Slack{Endpoint: server.URL, Client: server.Client(), Token: func() (string, error) { return "token", nil }}
			ts, err := s.Post(t.Context(), "C123", Post{Text: "Ad ready"})
			if err == nil || ts != "" || errors.Is(err, ErrNotPosted) != c.notPosted {
				t.Fatalf("ts %q, err %v: not-posted classification should be %v", ts, err, c.notPosted)
			}
		})
	}
	s := &Slack{Endpoint: "http://127.0.0.1:1", Client: http.DefaultClient, Token: func() (string, error) { return "", errors.New("runtime credential unavailable") }}
	if _, err := s.Post(t.Context(), "C123", Post{Text: "Ad ready"}); !errors.Is(err, ErrNotPosted) {
		t.Fatal("a post that never left the server was treated as uncertain", err)
	}
}

func TestSlackCarouselUploadFailurePostsNothing(t *testing.T) {
	var picture bytes.Buffer
	if err := png.Encode(&picture, image.NewGray(image.Rect(0, 0, 4, 3))); err != nil {
		t.Fatal(err)
	}
	post := Post{Text: "Resizes", Cards: []Card{{Title: "Source", Image: &CardImage{Data: "data:image/png;base64," + base64.StdEncoding.EncodeToString(picture.Bytes()), Alt: "Source"}}}}
	for _, failing := range []string{"/files.getUploadURLExternal", "/upload", "/files.completeUploadExternal"} {
		t.Run(failing, func(t *testing.T) {
			messages := 0
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == failing {
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				switch r.URL.Path {
				case "/files.getUploadURLExternal":
					w.Write([]byte(`{"ok":true,"file_id":"F1","upload_url":"` + server.URL + `/upload"}`))
				case "/chat.postMessage":
					messages++
					w.Write([]byte(`{"ok":true,"ts":"1.2"}`))
				default:
					w.Write([]byte(`{"ok":true}`))
				}
			}))
			defer server.Close()
			s := &Slack{Endpoint: server.URL, Client: server.Client(), Token: func() (string, error) { return "token", nil }}
			if _, err := s.Post(t.Context(), "C123", post); !errors.Is(err, ErrNotPosted) || messages != 0 {
				t.Fatalf("err %v after %d messages: a failed upload must post nothing and say so", err, messages)
			}
		})
	}
}

func TestSlackCarouselWaitsForUploadedPictures(t *testing.T) {
	var picture bytes.Buffer
	if err := png.Encode(&picture, image.NewGray(image.Rect(0, 0, 4, 3))); err != nil {
		t.Fatal(err)
	}
	post := Post{Text: "Resizes", Cards: []Card{{Title: "Source", Image: &CardImage{Data: "data:image/png;base64," + base64.StdEncoding.EncodeToString(picture.Bytes()), Alt: "Source"}}}}
	// Slack's answer, seen live, when a message names a file it has not yet processed.
	unprocessed := `{"ok":false,"error":"invalid_blocks","response_metadata":{"messages":["[ERROR] invalid file type [json-pointer:/blocks/1/elements/0/hero_image/slack_file.id/slack_file]"]}}`
	for _, c := range []struct {
		name     string
		refusals []string
		posts    int
		posted   bool
	}{
		{"picture still processing", []string{unprocessed}, 2, true},
		{"blocks invalid for another reason", []string{`{"ok":false,"error":"invalid_blocks","response_metadata":{"messages":["[ERROR] must be more than 0 characters [json-pointer:/blocks/1/elements/0/title/text]"]}}`}, 1, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			messages := 0
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/files.getUploadURLExternal":
					w.Write([]byte(`{"ok":true,"file_id":"F1","upload_url":"` + server.URL + `/upload"}`))
				case "/chat.postMessage":
					if messages++; messages <= len(c.refusals) {
						w.Write([]byte(c.refusals[messages-1]))
						return
					}
					w.Write([]byte(`{"ok":true,"ts":"1.2"}`))
				default:
					w.Write([]byte(`{"ok":true}`))
				}
			}))
			defer server.Close()
			s := &Slack{Endpoint: server.URL, Client: server.Client(), Token: func() (string, error) { return "token", nil }}
			ts, err := s.Post(t.Context(), "C123", post)
			if (err == nil) != c.posted || messages != c.posts || c.posted && ts != "1.2" {
				t.Fatalf("ts %q, err %v after %d messages", ts, err, messages)
			}
			if err != nil && (!errors.Is(err, ErrNotPosted) || !strings.Contains(err.Error(), "must be more than 0 characters")) {
				t.Fatal("refusal lost its classification or reason:", err)
			}
		})
	}
}

func TestSlackRespondsOnlyToSlackWebhooks(t *testing.T) {
	calls := 0
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer other.Close()
	s := &Slack{Endpoint: "http://127.0.0.1:1/chat.postEphemeral", Client: http.DefaultClient}
	for _, target := range []string{other.URL + "/actions/T1/1/x", "https://hooks.slack.com.example.org/actions/T1", "http://hooks.slack.com/actions/T1", "https://user@hooks.slack.com/actions/T1"} {
		if err := s.Respond(t.Context(), target, "Received."); err == nil {
			t.Fatalf("responded to %s", target)
		}
	}
	if calls != 0 {
		t.Fatal("a response URL from the payload reached another host")
	}
}
