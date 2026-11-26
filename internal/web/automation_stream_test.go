package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/workspace"
)

func TestAutomationBufferedStreamStillRejectsChangedEvidenceAndAccess(t *testing.T) {
	for _, mode := range []string{"content", "member", "additional notebook"} {
		t.Run(mode, func(t *testing.T) {
			var change func() error
			canceled := make(chan struct{})
			app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if err := change(); err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Must not be delivered.\"}}]}\n\n")
				w.(http.Flusher).Flush()
				if mode != "content" {
					<-r.Context().Done()
					close(canceled)
					return
				}
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			})
			config := *app.chat.Config
			config.Connections = append([]chat.Connection(nil), config.Connections...)
			config.Connections[0].Capabilities.Stream = true
			raw, _ := json.Marshal(config)
			file := filepath.Join(t.TempDir(), "chat.json")
			if err := os.WriteFile(file, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := app.ConfigureChat(file); err != nil {
				t.Fatal(err)
			}
			store, _ := app.catalog.Access(catalog.Bootstrap, "default", "edit")
			doc, err := store.Save(t.Context(), "source.md", []byte("The count is seven."), "", "test")
			if err != nil {
				t.Fatal(err)
			}
			run := automation.Run{Spec: automation.Spec{ID: "stream", Actor: "alice", Project: "default", AIConnection: "local", Model: "synthetic", ExternalProcessing: true}, Citations: []chat.Citation{{Kind: "notebook", Project: "default", Path: doc.Path, Revision: doc.Revision, Hash: workspace.ContentHash(doc.Content)}}}
			switch mode {
			case "content":
				change = func() error {
					_, err := store.Save(t.Context(), doc.Path, []byte("The count changed."), doc.Revision, "test")
					return err
				}
			case "member":
				change = func() error { return app.catalog.Member(catalog.Bootstrap, "default", "alice", "") }
			case "additional notebook":
				if err := app.catalog.Create(catalog.Bootstrap, "reference", "Reference"); err != nil {
					t.Fatal(err)
				}
				if err := app.catalog.Member(catalog.Bootstrap, "reference", "alice", "read"); err != nil {
					t.Fatal(err)
				}
				run.Spec.ReadProjects = []string{"reference"}
				change = func() error { return app.catalog.Member(catalog.Bootstrap, "reference", "alice", "") }
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			ctx, err = app.automationContext(ctx, run.Spec)
			if err != nil {
				t.Fatal(err)
			}
			answer, err := app.automationrun.Answer(ctx, &run, "Summarize the source", false)
			if err == nil || answer != "" || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
				t.Fatalf("changed evidence/access must reject the buffered answer without waiting for a timeout: %q %v, context %v", answer, err, ctx.Err())
			}
			if mode != "content" {
				select {
				case <-canceled:
				case <-ctx.Done():
					t.Fatal("revocation did not cancel the ongoing provider response")
				}
			}
		})
	}
}

// A buffered streamed reply assembles the same answer however the provider
// fragments it. The test once also counted the Git commands the evidence
// checks ran per fragment; the notes store answers those reads from memory,
// with no command to count.
func TestAutomationBufferedStreamAssemblesFragmentedReplies(t *testing.T) {
	for _, fragments := range []int{1, 40} {
		t.Run(fmt.Sprint(fragments), func(t *testing.T) {
			app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				for range fragments {
					fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", strings.Repeat("x", 40/fragments))
				}
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			})
			config := *app.chat.Config
			config.Connections = append([]chat.Connection(nil), config.Connections...)
			config.Connections[0].Capabilities.Stream = true
			config.Connections[0].Limits.RunSeconds = 60
			raw, _ := json.Marshal(config)
			file := filepath.Join(t.TempDir(), "chat.json")
			if err := os.WriteFile(file, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := app.ConfigureChat(file); err != nil {
				t.Fatal(err)
			}
			store, err := app.catalog.Access(catalog.Bootstrap, "default", "edit")
			if err != nil {
				t.Fatal(err)
			}
			doc, err := store.Save(t.Context(), "source.md", []byte("The count is seven."), "", "test")
			if err != nil {
				t.Fatal(err)
			}
			run := automation.Run{Spec: automation.Spec{ID: "stream", Actor: "alice", Project: "default", AIConnection: "local", Model: "synthetic", ExternalProcessing: true}, Citations: []chat.Citation{{Kind: "notebook", Project: "default", Path: doc.Path, Revision: doc.Revision, Hash: workspace.ContentHash(doc.Content)}}}
			ctx, err := app.automationContext(t.Context(), run.Spec)
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			answer, err := app.automationrun.Answer(ctx, &run, "Summarize the source", false)
			t.Logf("fragments=%d elapsed=%s", fragments, time.Since(start))
			if err != nil || answer != strings.Repeat("x", 40) {
				t.Fatalf("buffered answer: %q %v", answer, err)
			}
		})
	}
}
