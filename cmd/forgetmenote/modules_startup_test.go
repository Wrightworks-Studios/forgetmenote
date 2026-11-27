package main

import (
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// instanceFile writes a private instance.json for a gateway that serves a
// fresh database from dir on address, with modules as its modules section.
func instanceFile(t *testing.T, dir, address, modules string) string {
	t.Helper()
	server := `{"data":` + strconv.Quote(dir) + `,"listen":` + strconv.Quote(address) + `,"origin":` + strconv.Quote("http://"+address) + `,"database":{"urlFile":` + strconv.Quote(writeDatabaseURL(t, true)) + `}}`
	raw := `{"version":1,"server":` + server
	if modules != "" {
		raw += `,"modules":` + modules
	}
	file := filepath.Join(t.TempDir(), "instance.json")
	if err := os.WriteFile(file, []byte(raw+"}"), 0600); err != nil {
		t.Fatal(err)
	}
	return file
}

// Until a module's wiring lands, a gateway configured to turn it off does not
// start, rather than run a module the operator meant to be off.
func TestModulesOffStopsStartup(t *testing.T) {
	t.Setenv("INSTANCE_CONFIG_FILE", instanceFile(t, t.TempDir(), "127.0.0.1:0", `{"off":["personal","messaging","tasks"]}`))
	if err := runArguments(nil); err == nil || !strings.Contains(err.Error(), "turning these modules off is not supported yet") {
		t.Fatalf("startup accepted modules off: %v", err)
	}
}

// A gateway with an absent or empty modules section starts as before, runs
// every module and logs that set once. One that turns wired modules off starts
// without them and logs that.
func TestEveryModuleRunsAndIsLoggedWithoutModulesOff(t *testing.T) {
	const every = "ForgetMeNote modules: on: chat, automations, judgments, data-connections, sources, repositories, tasks, messaging, personal, memory, managed-backups; off: none"
	for section, line := range map[string]string{
		"":                                every,
		`{"off":[]}`:                      every,
		`{"off":["sources","judgments"]}`: "ForgetMeNote modules: on: chat, automations, data-connections, repositories, tasks, messaging, personal, memory, managed-backups; off: judgments, sources",
	} {
		dir := t.TempDir()
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := listener.Addr().String()
		listener.Close()
		logPath := filepath.Join(dir, "startup.log")
		output, err := os.Create(logPath)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestRetrievalStartupChild$")
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "FMN_RETRIEVAL_STARTUP_CHILD=1", "INSTANCE_CONFIG_FILE=" + instanceFile(t, dir, address, section)}
		cmd.Stdout = output
		cmd.Stderr = output
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		logs := func() string { raw, _ := os.ReadFile(logPath); return string(raw) }
		healthy := false
		client := &http.Client{Timeout: time.Second}
		for deadline := time.Now().Add(10 * time.Second); !healthy && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if response, err := client.Get("http://" + address + "/healthz"); err == nil {
				response.Body.Close()
				healthy = response.StatusCode == 200
			}
		}
		cmd.Process.Signal(os.Interrupt)
		select {
		case err = <-done:
		case <-time.After(4 * time.Second):
			cmd.Process.Kill()
			err = <-done
			t.Error("gateway did not shut down after interrupt")
		}
		output.Close()
		if !healthy || err != nil {
			t.Fatalf("section %q: gateway did not start and stop: %v %s", section, err, logs())
		}
		if strings.Count(logs(), line) != 1 {
			t.Fatalf("section %q: startup log does not show the modules once: %s", section, logs())
		}
	}
}
