package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
)

func TestAutomationPreservesSafeProviderFailure(t *testing.T) {
	for _, tc := range []struct {
		name, body, message string
		status              int
		kind                error
	}{
		{"unavailable", "secret upstream diagnostic", "HTTP Service Unavailable", 503, chat.ErrProviderRequest},
		{"incomplete", `{"choices":[{"message":{"role":"assistant","content":"partial"},"finish_reason":"length"}]}`, "did not complete", 200, chat.ErrProviderContract},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
				t.Fatal(err)
			}
			run := automation.Run{ID: automation.Hash(tc.name), Spec: automation.Spec{ID: "provider-failure", Kind: "task", Actor: "alice", Project: "default", AIConnection: "local", Output: "summary.md", Instruction: "Summarize", ExternalProcessing: true}}
			err := app.automationrun.RunAutomation(t.Context(), &run, func() error { return nil })
			if !errors.Is(err, tc.kind) || !strings.Contains(err.Error(), tc.message) || strings.Contains(err.Error(), tc.body) || calls != 1 {
				t.Fatalf("lost safe provider failure: calls=%d error=%v", calls, err)
			}
			store, _ := app.catalog.Access("alice", "default", "read")
			if _, err := store.Read(t.Context(), "summary.md", ""); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed provider response was published", err)
			}
		})
	}
}

func TestAutomationOutputFitsConfiguredAndRemainingBudgets(t *testing.T) {
	for _, tc := range []struct {
		name, wantError string
		output, budget  int
		min, max        int
	}{
		{"configured", "", 12000, 40000, 12000, 12000},
		{"remaining", "", 24000, 20000, 8193, 10999},
		{"exhausted", "automation token budget reached", 24000, 9000, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				var request struct {
					MaxTokens int `json:"max_tokens"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				if request.MaxTokens < tc.min || request.MaxTokens > tc.max {
					t.Errorf("output allowance=%d, want %d..%d", request.MaxTokens, tc.min, tc.max)
				}
				fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"# Complete"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20}}`)
			})
			if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
				t.Fatal(err)
			}
			config := *app.chat.Config
			config.Connections = append([]chat.Connection(nil), config.Connections...)
			config.Connections[0].Limits.ContextTokens = 100000
			config.Connections[0].Limits.OutputTokens = tc.output
			config.Connections[0].Capabilities.Tools = false
			raw, _ := json.Marshal(config)
			file := filepath.Join(t.TempDir(), "chat.json")
			if err := os.WriteFile(file, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := app.ConfigureChat(file); err != nil {
				t.Fatal(err)
			}
			run := automation.Run{ID: automation.Hash(tc.name), InputTokens: 7000, OutputTokens: 2000, Spec: automation.Spec{ID: "budget", Kind: "task", Actor: "alice", Project: "default", AIConnection: "local", Output: "summary.md", Instruction: "Summarize", ExternalProcessing: true, MaxTokens: tc.budget}}
			err := app.automationrun.RunAutomation(t.Context(), &run, func() error { return nil })
			if tc.wantError != "" {
				if err == nil || err.Error() != tc.wantError || calls != 0 {
					t.Fatalf("exhausted job purchased another request: calls=%d error=%v", calls, err)
				}
				return
			}
			if err != nil || calls != 1 {
				t.Fatal("bounded task failed", calls, err)
			}
			store, _ := app.catalog.Access("alice", "default", "read")
			doc, err := store.Read(t.Context(), "summary.md", "")
			if err != nil || !strings.Contains(string(doc.Content), "# Complete") {
				t.Fatal("complete answer was not published", err)
			}
		})
	}
}
