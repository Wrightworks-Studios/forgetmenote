package automation

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forgetmenote/internal/sandbox"
)

// The probe reports which accesses its run allowed. The host round trip that
// supplies the targets is the same pipe contract every package uses.
const pythonSandboxProbe = `import os, socket, subprocess, sys
from pathlib import Path

def attempt(action):
    try:
        action()
        return 'allowed'
    except (OSError, subprocess.CalledProcessError):
        return 'denied'

def main(host):
    paths = host.call('paths', 'checkpoint.get')
    here = Path(__file__).parent
    def plant():
        # Same-UID code can change permissions; Landlock still denies the write.
        os.chmod(here, 0o755)
        (here / 'planted').write_text('changed package')
    def tcp():
        socket.create_connection(tuple(paths['tcp']), timeout=5).close()
    def udp():
        with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as s:
            s.sendto(b'x', tuple(paths['udp']))
    return {
        'read-package': attempt(lambda: (here / 'main.py').read_text()),
        'read-credentials': attempt(lambda: Path(paths['credentials']).read_text()),
        'truncate-credentials': attempt(lambda: os.truncate(paths['credentials'], 0)),
        'read-notebook': attempt(lambda: Path(paths['notebook']).read_text()),
        'list-data': attempt(lambda: os.listdir(paths['data'])),
        'read-other-package': attempt(lambda: Path(paths['other']).read_text()),
        'write-package': attempt(plant),
        'write-scratch': attempt(lambda: Path('output').write_text('ok')),
        'tcp': attempt(tcp),
        'udp': attempt(udp),
        'subprocess': attempt(lambda: subprocess.run([sys.executable, '-I', '-S', '-c', 'pass'], check=True)),
    }
`

func runPythonProbe(t *testing.T, p *Python, id, digest string, sandboxed bool, paths map[string]any) (map[string]string, error) {
	t.Helper()
	var result map[string]string
	err := p.Run(t.Context(), id, digest, sandboxed, func(raw []byte) (any, error) {
		var call pythonCall
		if err := DecodeScriptRequest(raw, &call); err != nil {
			return nil, err
		}
		switch call.Operation {
		case "checkpoint.get":
			return paths, nil
		case "run.complete":
			var args struct {
				Result map[string]string `json:"result"`
			}
			err := json.Unmarshal(call.Arguments, &args)
			result = args.Result
			return true, err
		}
		return nil, fmt.Errorf("unexpected call %s", call.Operation)
	})
	if err == nil && result == nil {
		err = errors.New("probe did not complete")
	}
	return result, err
}

func TestPythonSandboxConfinesPackageToItsGrants(t *testing.T) {
	if err := sandbox.Available(); err != nil {
		t.Skip(err)
	}
	data := t.TempDir()
	p, err := OpenPython(t.Context(), filepath.Join(data, "automations", "python"), "python3", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = removePythonPreparation(p.root) })
	credentials, notebook := filepath.Join(data, "oauth.json"), filepath.Join(data, "projects", "finance", "ledger.md")
	if err = os.MkdirAll(filepath.Dir(notebook), 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{credentials: "refresh-token", notebook: "# Ledger"} {
		if err = os.WriteFile(name, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	otherID, _ := preparePython(t, p, pythonFiles("def main(host): return 'other'", ""))
	id, digest := preparePython(t, p, pythonFiles(pythonSandboxProbe, ""))
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	address := func(a net.Addr) []any {
		host, port, _ := net.SplitHostPort(a.String())
		var number int
		fmt.Sscan(port, &number)
		return []any{host, number}
	}
	paths := map[string]any{"data": data, "credentials": credentials, "notebook": notebook, "other": filepath.Join(p.root, otherID, "source", "main.py"), "tcp": address(tcp.Addr()), "udp": address(udp.LocalAddr())}

	got, err := runPythonProbe(t, p, id, digest, true, paths)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"read-package": "allowed", "write-scratch": "allowed", "subprocess": "allowed",
		"read-credentials": "denied", "truncate-credentials": "denied", "read-notebook": "denied", "list-data": "denied", "read-other-package": "denied", "write-package": "denied", "tcp": "denied", "udp": "denied"}
	for name, state := range want {
		if got[name] != state {
			t.Errorf("sandboxed %s = %q, want %q (all %v)", name, got[name], state, got)
		}
	}
	if _, err = os.Stat(filepath.Join(p.root, id, "source", "planted")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("sandboxed package changed its prepared version", err)
	}
	if raw, err := os.ReadFile(credentials); err != nil || string(raw) != "refresh-token" {
		t.Fatalf("sandboxed package changed instance credentials: %q %v", raw, err)
	}
	// Without the sandbox, the run keeps today's server-uid access.
	got, err = runPythonProbe(t, p, id, digest, false, paths)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"read-credentials", "read-notebook", "write-package", "tcp"} {
		if got[name] != "allowed" {
			t.Errorf("unsandboxed %s = %q, want allowed (all %v)", name, got[name], got)
		}
	}
}

// An administrator who asked for the sandbox must not get an unconfined run.
func TestPythonSandboxRefusesUnconfinedRun(t *testing.T) {
	if sandbox.Available() == nil {
		t.Skip("this kernel enforces the sandbox")
	}
	p := pythonFixture(t, "")
	id, digest := preparePython(t, p, pythonFiles("def main(host): return 'ran'", ""))
	err := p.Run(t.Context(), id, digest, true, func([]byte) (any, error) {
		t.Error("package ran without the sandbox it requested")
		return true, nil
	})
	if err == nil || !strings.Contains(err.Error(), "Landlock") {
		t.Fatalf("unconfined run was not refused: %v", err)
	}
	if got := runPythonResult(t, p, id, digest, false); got != "ran" {
		t.Fatal("unsandboxed run of the same version failed", got)
	}
}
