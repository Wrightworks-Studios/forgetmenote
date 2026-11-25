package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/knowledge"
)

// Run the real main/startup path in a separate process, including environment
// configuration, repository locks, HTTP service and graceful signal shutdown.
func TestRetrievalStartupChild(t *testing.T) {
	if os.Getenv("FMN_RETRIEVAL_STARTUP_CHILD") != "1" {
		t.Skip("subprocess entry point")
	}
	args := flag.Args()
	flag.CommandLine = flag.NewFlagSet("forgetmenote", flag.ExitOnError)
	os.Args = append([]string{"forgetmenote"}, args...)
	main()
	os.Exit(0)
}

func TestRetrievalSettingsStartupIsolation(t *testing.T) {
	for _, tc := range []struct{ name, retrieval, knowledge, wantFatal string }{
		{name: "unsupported", retrieval: `{"version":2}`},
		{name: "malformed", retrieval: `{"version":"1"}`},
		{name: "unrelated invalid config remains fatal", retrieval: `{"version":2}`, knowledge: `{"config":{"enabled":true,"model":"","dimensions":-1}}`, wantFatal: "choose a model identity"},
		{name: "unrelated unreadable knowledge remains fatal", retrieval: `{"version":2}`, knowledge: `{"config":"unreadable"}`, wantFatal: "knowledge state is unreadable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			database := databaseURLFile(t, true)
			db := instanceDatabase(t, database)
			projects, err := catalog.Open(dir, nil, db)
			if err != nil {
				t.Fatal(err)
			}
			if err = projects.Create(catalog.Bootstrap, "bad", "Bad settings"); err != nil {
				projects.Close()
				t.Fatal(err)
			}
			for _, id := range []string{"default", "bad"} {
				store, err := projects.Access(catalog.Bootstrap, id, "edit")
				if err != nil {
					projects.Close()
					t.Fatal(err)
				}
				content := "# Healthy startupneedle\n"
				if id == "bad" {
					content = "---\nfmn-container: archive\n---\n# Bad startupneedle\n"
				}
				if _, err = store.Save(context.Background(), "note.md", []byte(content), "", "synthetic startup"); err != nil {
					projects.Close()
					t.Fatal(err)
				}
			}
			projects.Close()
			if _, err = db.WriteDocument(context.Background(), db.Pool, "knowledge-retrieval", "bad", 0, []byte(tc.retrieval)); err != nil {
				t.Fatal(err)
			}
			if tc.knowledge != "" {
				if _, err = db.WriteDocument(context.Background(), db.Pool, "knowledge", "bad", 0, []byte(tc.knowledge)); err != nil {
					t.Fatal(err)
				}
			}
			const token = "synthetic-startup-admin-0123456789abcdefghijklmnopqrstuvwxyz"
			if err = os.WriteFile(filepath.Join(dir, "admin.token"), []byte(token), 0600); err != nil {
				t.Fatal(err)
			}
			var providerCalls atomic.Int32
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				providerCalls.Add(1)
				http.Error(w, "unexpected synthetic provider request", 503)
			}))
			defer endpoint.Close()
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			address := listener.Addr().String()
			listener.Close()
			origin := "http://" + address
			logPath := filepath.Join(dir, "startup.log")
			output, err := os.Create(logPath)
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			cmd := exec.Command(os.Args[0], "-test.run=^TestRetrievalStartupChild$", "--", "-data", dir, "-listen", address, "-url", origin)
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "FMN_RETRIEVAL_STARTUP_CHILD=1", "EMBEDDING_URL=" + endpoint.URL + "/api/embed", "DATABASE_URL_FILE=" + database}
			cmd.Stdout = output
			cmd.Stderr = output
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			var waitErr error
			go func() { waitErr = cmd.Wait(); close(done) }()
			logs := func() string { raw, _ := os.ReadFile(logPath); return string(raw) }
			t.Cleanup(func() {
				cmd.Process.Signal(os.Interrupt)
				select {
				case <-done:
					if tc.wantFatal == "" && waitErr != nil {
						t.Errorf("gateway shutdown: %v %s", waitErr, logs())
					}
				case <-time.After(4 * time.Second):
					cmd.Process.Kill()
					<-done
					t.Error("gateway did not shut down after interrupt")
				}
			})
			client := &http.Client{Timeout: time.Second}
			healthy := false
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
				select {
				case <-done:
					if tc.wantFatal != "" {
						if waitErr == nil || !strings.Contains(logs(), tc.wantFatal) {
							t.Fatalf("unrelated startup error changed: %v %s", waitErr, logs())
						}
						return
					}
					t.Fatalf("malformed notebook aborted configured gateway startup: %v %s", waitErr, logs())
				default:
				}
				response, err := client.Get(origin + "/healthz")
				if err == nil {
					response.Body.Close()
					if response.StatusCode == 200 {
						healthy = true
						break
					}
				}
				time.Sleep(20 * time.Millisecond)
			}
			if !healthy {
				t.Fatalf("gateway did not start: %s", logs())
			}
			if tc.wantFatal != "" {
				t.Fatalf("unrelated invalid knowledge configuration was suppressed: %s", logs())
			}
			request := func(method, path, body string, wantOK bool) string {
				t.Helper()
				r, err := http.NewRequest(method, origin+path, strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				r.Header.Set("Authorization", "Bearer "+token)
				r.Header.Set("Content-Type", "application/json")
				response, err := client.Do(r)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				raw, err := io.ReadAll(response.Body)
				if err != nil {
					t.Fatal(err)
				}
				if (response.StatusCode == 200) != wantOK {
					t.Fatalf("%s %s: %d %s", method, path, response.StatusCode, raw)
				}
				return string(raw)
			}
			if got := request("GET", "/p/default/api/notes?q=startupneedle", "", true); !strings.Contains(got, "note.md") {
				t.Fatal("healthy literal search unavailable", got)
			}
			if got := request("POST", "/p/default/api/knowledge/hybrid-search", `{"text":"startupneedle","mode":"text"}`, true); !strings.Contains(got, "note.md") {
				t.Fatal("healthy hybrid search unavailable", got)
			}
			if got := request("GET", "/p/bad/api/note?path=note.md", "", true); !strings.Contains(got, "Bad startupneedle") {
				t.Fatal("ordinary Read unavailable", got)
			}
			for _, path := range []string{"/p/bad/api/notes?collection=archives", "/p/bad/api/browse?collection=archives", "/p/bad/api/collections"} {
				if got := request("GET", path, "", true); !strings.Contains(got, "archive") {
					t.Fatal("collection unavailable", got)
				}
			}
			for _, path := range []string{"/p/bad/api/notes?q=startupneedle&archives=include", "/p/bad/api/knowledge/retrieval-settings"} {
				if got := request("GET", path, "", false); !strings.Contains(got, "retrieval settings") {
					t.Fatal("missing actionable retrieval failure", got)
				}
			}
			if got := request("POST", "/p/bad/api/knowledge/hybrid-search", `{"text":"startupneedle","mode":"text","archives":"include"}`, false); !strings.Contains(got, "retrieval settings") {
				t.Fatal(got)
			}
			version, raw, err := db.ReadDocument(context.Background(), db.Pool, "knowledge-retrieval", "bad", -1)
			if err != nil || version != 1 {
				t.Fatalf("bad settings replaced by defaults: %q %v", raw, err)
			}
			// A failed eager open is not cached as a permissive engine or permanent outage.
			repaired, _ := json.Marshal(knowledge.DefaultRetrievalSettings())
			if _, err = db.WriteDocument(context.Background(), db.Pool, "knowledge-retrieval", "bad", version, repaired); err != nil {
				t.Fatal(err)
			}
			// The note is archived, which ordinary search leaves out by default.
			if got := request("GET", "/p/bad/api/notes?q=startupneedle&archives=include", "", true); !strings.Contains(got, "note.md") {
				t.Fatal("repaired notebook did not recover", got)
			}
			if providerCalls.Load() != 0 {
				t.Fatalf("unexpected provider calls: %d", providerCalls.Load())
			}
		})
	}
}
