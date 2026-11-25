package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A misspelt setting must not start a copy that acts outward.
func TestUnknownOutgoingModeStopsStartup(t *testing.T) {
	databaseURLFile(t, true)
	t.Setenv("DATA_DIR", t.TempDir())
	t.Setenv("LISTEN_ADDR", "127.0.0.1:0")
	t.Setenv("OUTGOING", "pause")
	if err := runArguments(nil); err == nil || !strings.Contains(err.Error(), `outgoing must be "paused"`) {
		t.Fatalf("startup accepted an unknown outgoing mode: %v", err)
	}
}

// An operator sees a paused copy in its startup log and administration status.
func TestOutgoingPausedStartupIsVisible(t *testing.T) {
	dir := t.TempDir()
	database := databaseURLFile(t, true)
	const token = "synthetic-outgoing-admin-0123456789abcdefghijklmnopqrstuvwxyz"
	if err := os.WriteFile(filepath.Join(dir, "admin.token"), []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
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
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "FMN_RETRIEVAL_STARTUP_CHILD=1", "DATABASE_URL_FILE=" + database, "OUTGOING=paused"}
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
			if waitErr != nil {
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
	for deadline := time.Now().Add(10 * time.Second); !healthy && time.Now().Before(deadline); {
		select {
		case <-done:
			t.Fatalf("paused gateway exited: %v %s", waitErr, logs())
		default:
		}
		if response, err := client.Get(origin + "/healthz"); err == nil {
			response.Body.Close()
			healthy = response.StatusCode == 200
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !healthy {
		t.Fatalf("paused gateway did not start: %s", logs())
	}
	if !strings.Contains(logs(), "outgoing actions are paused by server.outgoing (OUTGOING)") {
		t.Fatalf("startup log does not show the pause: %s", logs())
	}
	r, _ := http.NewRequest("GET", origin+"/api/admin", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var admin struct {
		Instance map[string]bool `json:"instance"`
	}
	if err = json.NewDecoder(response.Body).Decode(&admin); err != nil || response.StatusCode != 200 || !admin.Instance["outgoingPaused"] {
		t.Fatalf("administration status does not show the pause: %d %+v %v", response.StatusCode, admin, err)
	}
}
