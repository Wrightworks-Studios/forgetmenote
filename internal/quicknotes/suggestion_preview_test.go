package quicknotes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/workspace"
)

func TestListRevalidatesSuggestedDestination(t *testing.T) {
	for _, change := range []string{"unchanged", "revoked", "folder-removed", "sharing-changed", "sharing-added"} {
		t.Run(change, func(t *testing.T) {
			s, c, _ := fixture(t)
			stopScanWorker(t, s)
			ctx := context.Background()
			if err := c.Create(catalog.Bootstrap, "target", "Confidential routing destination"); err != nil {
				t.Fatal(err)
			}
			if err := c.Member(catalog.Bootstrap, "target", "alice", "owner"); err != nil {
				t.Fatal(err)
			}
			if change == "sharing-changed" {
				if err := c.Member("alice", "target", "bob", "read"); err != nil {
					t.Fatal(err)
				}
			}
			target, err := c.Access("alice", "target", "owner")
			if err != nil {
				t.Fatal(err)
			}
			reference, err := target.Save(ctx, "planning/reference.md", []byte("Existing planning note."), "", "alice")
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			s.ConfigureJudgments(testJudge(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); routeReply(w, r, "destination_001") }), false)
			settings, err := s.Settings("alice")
			if err != nil {
				t.Fatal(err)
			}
			settings.CloudConsent = true
			if _, err = s.UpdateSettings("alice", settings); err != nil {
				t.Fatal(err)
			}
			const content = "# Capture\nProject planning for next week."
			capture, err := s.Capture(ctx, "alice", "suggestion-preview", content)
			if err != nil {
				t.Fatal(err)
			}
			suggested, err := s.Sort(ctx, "alice", capture.ID)
			if err != nil || suggested.Status != "suggested" || suggested.Suggestion == nil || suggested.Suggestion.Destination.Folder != "planning" || calls.Load() != 1 {
				t.Fatal("real suggestion fixture", suggested, err, calls.Load())
			}
			destination := suggested.Suggestion.Destination
			switch change {
			case "revoked":
				err = c.Member(catalog.Bootstrap, "target", "alice", "")
			case "folder-removed":
				_, err = target.Move(ctx, "planning", "elsewhere", reference.Revision, "alice")
			case "sharing-changed":
				err = c.Member("alice", "target", "bob", "")
			case "sharing-added":
				err = c.Member("alice", "target", "bob", "read")
			}
			if err != nil {
				t.Fatal("destination change", err)
			}
			available, err := s.Destinations(ctx, "alice")
			if err != nil {
				t.Fatal(err)
			}
			exactAvailable := false
			for _, d := range available {
				if d == destination {
					exactAvailable = true
				}
			}
			if exactAvailable != (change == "unchanged") {
				t.Fatal("destination fixture did not change exact current inventory", available)
			}
			for range 3 {
				listed, err := s.List(ctx, "alice")
				if err != nil || len(listed) != 1 || listed[0].ID != capture.ID || listed[0].Project != capture.InboxProject {
					t.Fatal("capture lost from private Inbox", listed, err)
				}
				got := listed[0]
				if change == "unchanged" {
					if got.Status != "suggested" || got.Suggestion == nil || got.Suggestion.Destination != destination {
						t.Fatal("authorized unchanged suggestion hidden", got)
					}
				} else {
					if got.Status != "inbox" || got.Suggestion != nil {
						t.Errorf("stale destination suggestion disclosed after %s: %+v", change, got)
					}
					encoded, err := json.Marshal(got)
					if err != nil {
						t.Fatal(err)
					}
					if strings.Contains(string(encoded), destination.Name) || strings.Contains(string(encoded), `"folder":"planning"`) {
						t.Errorf("inaccessible or changed destination metadata disclosed: %s", encoded)
					}
				}
			}
			if calls.Load() != 1 {
				t.Fatal("List classified or paid for a new judgment", calls.Load())
			}
			if change != "unchanged" {
				replay, err := s.Capture(ctx, "alice", "suggestion-preview", content)
				if err != nil || replay.Status != "inbox" || replay.Suggestion != nil {
					t.Fatal("capture replay exposed stale suggestion", replay, err)
				}
			}
			if change == "revoked" {
				if _, err = s.File(ctx, "alice", capture.ID, destination, suggested.Version); !errors.Is(err, workspace.ErrForbidden) {
					t.Fatal("revoked filing guard regressed", err)
				}
			}
			inbox, err := c.Access("alice", capture.InboxProject, "owner")
			if err != nil {
				t.Fatal(err)
			}
			current, err := inbox.Read(ctx, capture.Path, "")
			if err != nil || string(current.Content) != content || current.Version != capture.Version {
				t.Fatal("preview changed captured bytes or revision", err)
			}
			history, err := inbox.History(ctx, capture.Path)
			if err != nil || len(history) != 1 {
				t.Fatal("preview changed captured Git history", len(history), err)
			}
		})
	}
}
