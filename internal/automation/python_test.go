package automation

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"forgetmenote/internal/sandbox"
)

func pythonFixture(t *testing.T, wheelhouse string) *Python {
	t.Helper()
	p, err := OpenPython(t.Context(), filepath.Join(t.TempDir(), "python"), "python3", wheelhouse)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := removePythonPreparation(p.root); err != nil {
			t.Error(err)
		}
	})
	return p
}

func pythonFiles(code, lock string) map[string]string {
	m := Manifest{APIVersion: PackageAPI, ID: "fixture", Name: "Fixture", Runtime: PythonRuntime, Entrypoint: "main.py", Files: []string{"main.py"}}
	files := map[string]string{"main.py": code}
	if lock != "" {
		m.Requirements = "requirements.lock"
		m.Files = append(m.Files, m.Requirements)
		files[m.Requirements] = lock
	}
	raw, _ := json.Marshal(m)
	files["automation.json"] = string(raw)
	return files
}

func preparePython(t *testing.T, p *Python, files map[string]string) (string, string) {
	t.Helper()
	digest := PackageDigest(files)
	id, err := p.Prepare(t.Context(), files, digest)
	if err != nil {
		t.Fatal(err)
	}
	return id, digest
}

type pythonCall struct {
	Step      string          `json:"step"`
	Operation string          `json:"operation"`
	Arguments json.RawMessage `json:"arguments"`
}

func runPythonResult(t *testing.T, p *Python, id, digest string, sandboxed bool) string {
	t.Helper()
	var result string
	err := p.Run(t.Context(), id, digest, sandboxed, func(raw []byte) (any, error) {
		var call pythonCall
		if err := DecodeScriptRequest(raw, &call); err != nil {
			return nil, err
		}
		if call.Operation != "run.complete" {
			return nil, fmt.Errorf("unexpected call %s", call.Operation)
		}
		var args struct {
			Result string `json:"result"`
		}
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return nil, err
		}
		result = args.Result
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestPythonRunRequiresExplicitPreparation(t *testing.T) {
	p := pythonFixture(t, "")
	files := pythonFiles("def main(host): return 'stdlib'", "")
	digest := PackageDigest(files)
	if err := p.Run(t.Context(), p.environmentID(digest), digest, false, nil); err == nil {
		t.Fatal("run implicitly prepared a package")
	}
	id, digest := preparePython(t, p, files)
	if _, err := os.Stat(filepath.Join(p.root, id, "venv")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stdlib package created a venv: %v", err)
	}
	if got := runPythonResult(t, p, id, digest, false); got != "stdlib" {
		t.Fatal(got)
	}
}

func TestPythonProtocolAndDiagnostics(t *testing.T) {
	p := pythonFixture(t, "")
	t.Run("malformed stdout", func(t *testing.T) {
		id, digest := preparePython(t, p, pythonFiles("import os, time\ndef main(host):\n os.write(1, b'not protocol\\n')\n time.sleep(60)", ""))
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		err := p.Run(ctx, id, digest, false, func(raw []byte) (any, error) { var call pythonCall; return nil, DecodeScriptRequest(raw, &call) })
		if !errors.Is(err, ErrPythonProtocol) {
			t.Fatalf("expected original protocol error, got %v", err)
		}
		if ctx.Err() != nil {
			t.Fatal("protocol violation waited for deadline")
		}
	})
	t.Run("ordinary print and nonzero exit", func(t *testing.T) {
		t.Setenv("FMN_TEST_SECRET", "sensitive-fixture-value")
		id, digest := preparePython(t, p, pythonFiles("def main(host):\n print('context before failure')\n raise RuntimeError('sensitive-fixture-value')", ""))
		err := p.Run(t.Context(), id, digest, false, func(raw []byte) (any, error) { t.Error("diagnostic escaped onto protocol pipe"); return nil, nil })
		if err == nil || !strings.Contains(err.Error(), "context before failure") || !strings.Contains(err.Error(), "RuntimeError") || strings.Contains(err.Error(), "sensitive-fixture-value") {
			t.Fatalf("unsafe or missing failure diagnostic: %v", err)
		}
	})
}

const pythonDescendants = `import os, subprocess, sys, time
def main(host):
 child = subprocess.Popen([sys.executable, '-I', '-S', '-c', 'import time; time.sleep(60)'])
 host.call('started', 'checkpoint.set', pids=[os.getpid(), child.pid])
 time.sleep(60)
`

func pythonPIDs(raw []byte) ([]int, error) {
	var call pythonCall
	if err := DecodeScriptRequest(raw, &call); err != nil {
		return nil, err
	}
	var args struct {
		PIDs []int `json:"pids"`
	}
	err := json.Unmarshal(call.Arguments, &args)
	return args.PIDs, err
}

func awaitStopped(t *testing.T, pids []int) {
	t.Helper()
	if len(pids) != 2 {
		t.Fatalf("missing child and descendant PIDs: %v", pids)
	}
	for _, pid := range pids {
		deadline := time.Now().Add(4 * time.Second)
		for {
			var state string
			if runtime.GOOS == "linux" {
				out, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
				// A process reaped between the open and the read fails the read with ESRCH.
				if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				_, fields, ok := strings.Cut(string(out), ") ")
				if !ok {
					t.Fatal("invalid process state")
				}
				state = strings.Fields(fields)[0]
			} else {
				out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
				var exited *exec.ExitError
				if errors.As(err, &exited) && exited.ExitCode() == 1 && len(out) == 0 {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				state = strings.TrimSpace(string(out))
			}
			// Linux container PID 1 may reap an orphan later; Z cannot run or do I/O.
			if state == "" || strings.HasPrefix(state, "Z") {
				break
			}
			if time.Now().After(deadline) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
				t.Fatalf("Python process %d survived cancellation (%s)", pid, state)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// sandboxModes are the run modes this host can exercise: the strict sandbox
// needs Linux Landlock, and a sandboxed run elsewhere is refused (tested below).
func sandboxModes() []bool {
	if sandbox.Available() != nil {
		return []bool{false}
	}
	return []bool{false, true}
}

func TestPythonCancellationStopsProcessTree(t *testing.T) {
	for _, sandboxed := range sandboxModes() {
		t.Run(fmt.Sprint("sandboxed=", sandboxed), func(t *testing.T) { testPythonCancellationStopsProcessTree(t, sandboxed) })
	}
}

func testPythonCancellationStopsProcessTree(t *testing.T, sandboxed bool) {
	p := pythonFixture(t, "")
	id, digest := preparePython(t, p, pythonFiles(pythonDescendants, ""))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var pids []int
	err := p.Run(ctx, id, digest, sandboxed, func(raw []byte) (any, error) {
		var err error
		pids, err = pythonPIDs(raw)
		cancel()
		return true, err
	})
	awaitStopped(t, pids)
	// As container PID 1, the server adopts orphaned descendants. Cancellation
	// must reap them too; otherwise repeated jobs accumulate zombie processes.
	var status syscall.WaitStatus
	if pid, err := syscall.Wait4(-pids[0], &status, syscall.WNOHANG, nil); err == nil && pid > 0 {
		t.Fatalf("cancelled run left an unreaped descendant: %d", pid)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost its cause: %v", err)
	}
}

func TestPythonParentDeathStopsProcessTree(t *testing.T) {
	for _, sandboxed := range sandboxModes() {
		t.Run(fmt.Sprint("sandboxed=", sandboxed), func(t *testing.T) { testPythonParentDeathStopsProcessTree(t, sandboxed) })
	}
}

func testPythonParentDeathStopsProcessTree(t *testing.T, sandboxed bool) {
	p := pythonFixture(t, "")
	id, digest := preparePython(t, p, pythonFiles(pythonDescendants, ""))
	pidFile := filepath.Join(t.TempDir(), "pids.json")
	cmd := exec.Command(os.Args[0], "-test.run=^TestPythonParentHelper$")
	cmd.Env = append(os.Environ(), "FMN_PYTHON_PARENT_HELPER="+pidFile, "FMN_PYTHON_ROOT="+p.root, "FMN_PYTHON_ID="+id, "FMN_PYTHON_DIGEST="+digest, "FMN_PYTHON_SANDBOX="+strconv.FormatBool(sandboxed))
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	var pids []int
	deadline := time.Now().Add(8 * time.Second)
	for {
		raw, err := os.ReadFile(pidFile)
		if err == nil && json.Unmarshal(raw, &pids) == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("child never started: %s", log.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	awaitStopped(t, pids)
	orphans, err := filepath.Glob(filepath.Join(p.root, "run-*"))
	if err != nil || len(orphans) != 1 {
		t.Fatalf("crash fixture lost its scratch directory: %v %v", orphans, err)
	}
	if _, err = OpenPython(t.Context(), p.root, "python3", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(orphans[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restart retained disposable crash scratch: %v", err)
	}
}

func TestPythonParentHelper(t *testing.T) {
	pidFile := os.Getenv("FMN_PYTHON_PARENT_HELPER")
	if pidFile == "" {
		t.Skip("subprocess helper")
	}
	p, err := OpenPython(t.Context(), os.Getenv("FMN_PYTHON_ROOT"), "python3", "")
	if err != nil {
		t.Fatal(err)
	}
	err = p.Run(t.Context(), os.Getenv("FMN_PYTHON_ID"), os.Getenv("FMN_PYTHON_DIGEST"), os.Getenv("FMN_PYTHON_SANDBOX") == "true", func(raw []byte) (any, error) {
		pids, err := pythonPIDs(raw)
		if err != nil {
			return nil, err
		}
		data, _ := json.Marshal(pids)
		return true, os.WriteFile(pidFile, data, 0600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPythonPinnedDependenciesPrepareOnceAndFailedUpdatePreservesVersion(t *testing.T) {
	wheelhouse := t.TempDir()
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	for name, content := range map[string]string{
		"fmn_fixture_dependency/__init__.py":            "VALUE = 'installed fixture'\n",
		"fmn_fixture_dependency-1.0.dist-info/METADATA": "Metadata-Version: 2.1\nName: fmn-fixture-dependency\nVersion: 1.0\n",
		"fmn_fixture_dependency-1.0.dist-info/WHEEL":    "Wheel-Version: 1.0\nGenerator: fmn-test\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
		"fmn_fixture_dependency-1.0.dist-info/RECORD":   "",
	} {
		f, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	wheel := filepath.Join(wheelhouse, "fmn_fixture_dependency-1.0-py3-none-any.whl")
	if err := os.WriteFile(wheel, buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	p := pythonFixture(t, wheelhouse)
	code := "import fmn_fixture_dependency\ndef main(host): return fmn_fixture_dependency.VALUE"
	files := pythonFiles(code, "fmn-fixture-dependency==1.0 --hash=sha256:"+hex.EncodeToString(sum[:]))
	id, digest := preparePython(t, p, files)
	readyPath := filepath.Join(p.root, id, "ready.json")
	before, err := os.Stat(readyPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, sandboxed := range sandboxModes() {
		if got := runPythonResult(t, p, id, digest, sandboxed); got != "installed fixture" {
			t.Fatal(sandboxed, got)
		}
	}
	if err = os.Remove(wheel); err != nil {
		t.Fatal(err)
	}
	// Re-preparation and runs succeed with the wheel unavailable: no pip at run time.
	if reused, err := p.Prepare(t.Context(), files, digest); err != nil || reused != id {
		t.Fatalf("reinstall instead of reuse: %s %v", reused, err)
	}
	after, _ := os.Stat(readyPath)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("prepared version was mutated")
	}
	if err = os.WriteFile(wheel, buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	bad := pythonFiles(code, "fmn-fixture-dependency==1.0 --hash=sha256:"+strings.Repeat("0", 64))
	badDigest := PackageDigest(bad)
	if _, err = p.Prepare(t.Context(), bad, badDigest); err == nil || !strings.Contains(err.Error(), "HASHES") {
		t.Fatalf("unverified dependency accepted: %v", err)
	}
	if err = p.Ready(p.environmentID(badDigest), badDigest); err == nil {
		t.Fatal("failed preparation marked ready")
	}
	if got := runPythonResult(t, p, id, digest, false); got != "installed fixture" {
		t.Fatal(got)
	}
}

func TestPythonUpdateDoesNotMutateRunningVersion(t *testing.T) {
	p := pythonFixture(t, "")
	id, digest := preparePython(t, p, pythonFiles("def main(host):\n host.call('wait', 'checkpoint.get')\n return 'old'", ""))
	var result string
	var newID, newDigest string
	err := p.Run(t.Context(), id, digest, false, func(raw []byte) (any, error) {
		var call pythonCall
		if err := DecodeScriptRequest(raw, &call); err != nil {
			return nil, err
		}
		if call.Operation == "checkpoint.get" {
			newID, newDigest = preparePython(t, p, pythonFiles("def main(host): return 'new'", ""))
		} else {
			var args struct {
				Result string `json:"result"`
			}
			_ = json.Unmarshal(call.Arguments, &args)
			result = args.Result
		}
		return true, nil
	})
	if err != nil || result != "old" || id == newID {
		t.Fatalf("running version changed: %s %v", result, err)
	}
	if got := runPythonResult(t, p, newID, newDigest, false); got != "new" {
		t.Fatal(got)
	}
}

// pythonWrapper returns a private executable that runs the host's python3, so
// a test can replace the configured interpreter.
func pythonWrapper(t *testing.T, comment string) string {
	t.Helper()
	real, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	if real, err = filepath.EvalSymlinks(real); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "python")
	next := filepath.Join(dir, "next")
	if err = os.WriteFile(next, []byte("#!/bin/sh\n# "+comment+"\nexec "+real+" \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPythonRunRefusesAReplacedInterpreter(t *testing.T) {
	executable := pythonWrapper(t, "original")
	p, err := OpenPython(t.Context(), filepath.Join(t.TempDir(), "python"), executable, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := removePythonPreparation(p.root); err != nil {
			t.Error(err)
		}
	})
	id, digest := preparePython(t, p, pythonFiles("def main(host): return 'stdlib'", ""))
	if got := runPythonResult(t, p, id, digest, false); got != "stdlib" {
		t.Fatal(got)
	}
	// An upgrade installs a new file over the configured path.
	next := filepath.Join(filepath.Dir(executable), "next")
	raw, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(next, append(raw, "# upgraded\n"...), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(next, executable); err != nil {
		t.Fatal(err)
	}
	err = p.Run(t.Context(), id, digest, false, func([]byte) (any, error) { return nil, nil })
	if err == nil || !strings.Contains(err.Error(), "configured Python changed") {
		t.Fatalf("a run used a replaced interpreter: %v", err)
	}
}

// BenchmarkPythonInterpreterCheck compares what each run once paid to confirm
// its interpreter, a probe and a hash of the binary, with the stat it pays now.
func BenchmarkPythonInterpreterCheck(b *testing.B) {
	p, err := OpenPython(b.Context(), filepath.Join(b.TempDir(), "python"), "python3", "")
	if err != nil {
		b.Skip(err)
	}
	b.Cleanup(func() { removePythonPreparation(p.root) })
	b.Run("probe", func(b *testing.B) {
		for b.Loop() {
			if _, _, err := p.interpreterIdentity(b.Context()); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("stat", func(b *testing.B) {
		for b.Loop() {
			if current, err := statInterpreter(p.executable); err != nil || current != p.interpreter {
				b.Fatal(err)
			}
		}
	})
}
