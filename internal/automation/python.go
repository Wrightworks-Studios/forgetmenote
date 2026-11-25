package automation

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"forgetmenote/internal/durable"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"

	"forgetmenote/internal/capacity"
	"forgetmenote/internal/procgroup"
	"forgetmenote/internal/sandbox"
)

//go:embed python/runner.py python/fmn_host.py
var pythonClient embed.FS

// Python owns prepared package files/environments and child process lifetimes.
// Scheduling, authority and durable call outcomes remain with Queue and the host.
// An unsandboxed run is trusted server code: this process boundary alone does not
// confine it. A sandboxed run is confined by the sandbox helper (see confine).
type Python struct {
	mu                                     sync.Mutex
	root, executable, identity, wheelhouse string
	prefix                                 string // the interpreter's installation (sys.base_prefix)
	// interpreter is the executable file identity was computed from. A run
	// compares a fresh stat with it instead of probing and hashing again.
	interpreter interpreterFile
}

// interpreterFile is what a stat shows of the interpreter's executable:
// replacing or rewriting it changes these fields. Its modification time is
// kept in nanoseconds, so equal files compare equal.
type interpreterFile struct {
	dev, inode uint64
	size       int64
	mode       os.FileMode
	modified   int64
}

func statInterpreter(path string) (interpreterFile, error) {
	info, err := os.Stat(path)
	if err != nil {
		return interpreterFile{}, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return interpreterFile{}, errors.New("the configured Python's file identity is unavailable")
	}
	return interpreterFile{dev: uint64(st.Dev), inode: uint64(st.Ino), size: info.Size(), mode: info.Mode(), modified: info.ModTime().UnixNano()}, nil
}

type pythonPreparation struct {
	Version      string `json:"version"`
	ID           string `json:"id"`
	Digest       string `json:"digest"`
	Python       string `json:"python"`
	Requirements string `json:"requirements,omitempty"`
	Entrypoint   string `json:"entrypoint"`
}

var ErrPythonProtocol = errors.New("malformed Python host request")

func OpenPython(ctx context.Context, root, executable, wheelhouse string) (*Python, error) {
	bin, err := exec.LookPath(executable)
	if err != nil {
		return nil, fmt.Errorf("configure a Python 3.12+ executable: %w", err)
	}
	bin, err = filepath.EvalSymlinks(bin)
	if err != nil {
		return nil, err
	}
	bin, err = filepath.Abs(bin)
	if err != nil {
		return nil, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("Python package directory must be private and not a symlink")
	}
	if wheelhouse != "" {
		wheelhouse, err = filepath.Abs(wheelhouse)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(wheelhouse)
		if err != nil || !info.IsDir() {
			return nil, errors.New("Python wheelhouse must be a local operator directory")
		}
	}
	p := &Python{root: root, executable: bin, wheelhouse: wheelhouse}
	if p.interpreter, err = statInterpreter(bin); err != nil {
		return nil, err
	}
	p.identity, p.prefix, err = p.interpreterIdentity(ctx)
	if err != nil {
		return nil, err
	}
	// A file replaced while it was probed must not pass for the probed one.
	if current, err := statInterpreter(bin); err != nil || current != p.interpreter {
		return nil, errors.Join(errors.New("configured Python changed while it was inspected"), err)
	}
	// The server owns this directory exclusively. A crash can leave scratch
	// files, but never a reusable run: recovery comes from Queue checkpoints.
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "run-") {
			if err = removePythonPreparation(filepath.Join(root, entry.Name())); err != nil {
				return nil, fmt.Errorf("remove abandoned Python scratch: %w", err)
			}
		}
	}
	return p, nil
}

func pythonCommand(ctx context.Context, executable, dir string, args ...string) *exec.Cmd {
	cmd := procgroup.Command(ctx, executable, args...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + filepath.Dir(executable) + ":/usr/bin:/bin", "HOME=" + dir, "TMPDIR=" + dir, "LANG=C.UTF-8", "PIP_CONFIG_FILE=/dev/null"}
	return cmd
}

// interpreterIdentity also returns the installation prefix a sandboxed run reads.
// The probe output is part of every prepared environment's identity: changing
// it would require re-preparing every installation.
func (p *Python) interpreterIdentity(ctx context.Context) (string, string, error) {
	cmd := pythonCommand(ctx, p.executable, p.root, "-I", "-S", "-c", `import json,sys; print(json.dumps({'version':list(sys.version_info[:3]),'prefix':sys.base_prefix,'implementation':sys.implementation.name}))`)
	raw, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("inspect configured Python: %w", err)
	}
	var info struct {
		Version []int  `json:"version"`
		Prefix  string `json:"prefix"`
	}
	if json.Unmarshal(raw, &info) != nil || len(info.Version) != 3 || info.Version[0] != 3 || info.Version[1] < 12 {
		return "", "", errors.New("automation packages require Python 3.12 or newer in the Python 3 series")
	}
	f, err := os.Open(p.executable)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", "", err
	}
	return Hash([]string{p.executable, hex.EncodeToString(h.Sum(nil)), string(raw)}), info.Prefix, nil
}

func (p *Python) environmentID(digest string) string {
	a, _ := pythonClient.ReadFile("python/runner.py")
	b, _ := pythonClient.ReadFile("python/fmn_host.py")
	return Hash([]string{PythonRuntime, p.identity, digest, string(a), string(b)})
}

// Require complete version/hash pins. Pip never resolves an undeclared transitive
// dependency, executes an sdist build, or follows an editable/VCS/local input.
var lockedRequirement = regexp.MustCompile(`(?i)^[a-z0-9][a-z0-9._-]*(\[[a-z0-9,._-]+\])?==[a-z0-9][a-z0-9.!+_-]*$`)

func validatePythonLock(lock string) error {
	lock = strings.ReplaceAll(strings.ReplaceAll(lock, "\r\n", "\n"), "\\\n", " ")
	count := 0
	for _, line := range strings.Split(lock, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 || !lockedRequirement.MatchString(parts[0]) {
			return errors.New("requirements must contain exact name==version pins and SHA-256 hashes, without directives or URLs")
		}
		for _, hash := range parts[1:] {
			digest, ok := strings.CutPrefix(hash, "--hash=sha256:")
			if !ok || !digestPattern.MatchString(digest) {
				return errors.New("each requirement must have only SHA-256 hash pins")
			}
		}
		count++
	}
	if count == 0 {
		return errors.New("empty requirements lock; omit requirements for standard-library packages")
	}
	return nil
}

func (p *Python) Prepare(ctx context.Context, files map[string]string, digest string) (id string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if PackageDigest(files) != digest {
		return "", errors.New("package digest mismatch")
	}
	m, err := ParseManifest([]byte(files["automation.json"]))
	if err != nil {
		return "", err
	}
	if len(files) != len(m.Files)+1 {
		return "", errors.New("package file set differs from manifest")
	}
	for _, name := range m.Files {
		if _, ok := files[name]; !ok {
			return "", fmt.Errorf("missing package file %s", name)
		}
	}
	if m.Requirements != "" {
		if err = validatePythonLock(files[m.Requirements]); err != nil {
			return "", err
		}
	}
	current, _, err := p.interpreterIdentity(ctx)
	if err != nil {
		return "", err
	}
	if current != p.identity {
		return "", errors.New("configured Python changed; restart ForgetMeNote before preparing a new environment")
	}
	id = p.environmentID(digest)
	if _, err = p.prepared(id, digest); err == nil {
		return id, nil
	}
	dir := filepath.Join(p.root, id)
	if _, statErr := os.Lstat(filepath.Join(dir, "ready.json")); statErr == nil {
		return "", fmt.Errorf("prepared environment is damaged: %w", err)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", statErr
	}
	// Only incomplete preparation is replaceable. A ready version is never edited.
	if err = removePythonPreparation(dir); err != nil {
		return "", err
	}
	if err = os.Mkdir(dir, 0700); err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, removePythonPreparation(dir))
		}
	}()
	for name, content := range files {
		if !PackagePath(name) {
			return "", errors.New("unsafe package path")
		}
		filename := filepath.Join(dir, "source", name)
		if err = os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
			return "", err
		}
		if err = os.WriteFile(filename, []byte(content), 0444); err != nil {
			return "", err
		}
	}
	if err = os.Mkdir(filepath.Join(dir, "client"), 0700); err != nil {
		return "", err
	}
	for _, name := range []string{"runner.py", "fmn_host.py"} {
		raw, _ := pythonClient.ReadFile("python/" + name)
		if err = os.WriteFile(filepath.Join(dir, "client", name), raw, 0444); err != nil {
			return "", err
		}
	}
	// Parse all Python without executing package code during installation.
	if err = runPythonPreparation(ctx, p.executable, dir, "-I", "-S", "-c", `import ast,pathlib,sys; [ast.parse(p.read_text(), filename=str(p)) for p in pathlib.Path(sys.argv[1]).rglob('*.py')]`, filepath.Join(dir, "source")); err != nil {
		return "", err
	}
	if m.Requirements != "" {
		if err = runPythonPreparation(ctx, p.executable, dir, "-I", "-m", "venv", "--copies", filepath.Join(dir, "venv")); err != nil {
			return "", err
		}
		bin := filepath.Join(dir, "venv", "bin", "python")
		args := []string{"-I", "-B", "-m", "pip", "--isolated", "--disable-pip-version-check", "--no-input", "install", "--require-hashes", "--only-binary=:all:", "--no-deps", "--no-compile", "--no-cache-dir", "-r", filepath.Join(dir, "source", m.Requirements)}
		if p.wheelhouse != "" {
			args = append(args, "--no-index", "--find-links", p.wheelhouse)
		} else {
			args = append(args, "--index-url", "https://pypi.org/simple")
		}
		if err = runPythonPreparation(ctx, bin, dir, args...); err != nil {
			return "", err
		}
		if err = runPythonPreparation(ctx, bin, dir, "-I", "-m", "pip", "--isolated", "check"); err != nil {
			return "", err
		}
	}
	ready := pythonPreparation{Version: PythonRuntime, ID: id, Digest: digest, Python: p.identity, Requirements: m.Requirements, Entrypoint: m.Entrypoint}
	raw, _ := json.Marshal(ready)
	if err = durable.WriteFile(filepath.Join(dir, "ready.json"), ".pending-", raw, durable.SyncDir); err != nil {
		return "", err
	}
	// Read-only files guard accidental modification; same-UID trusted code can
	// still chmod them. This is version management, not a security boundary.
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		mode := fs.FileMode(0444)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() || info.Mode().Perm()&0111 != 0 {
			mode = 0555
		}
		return os.Chmod(path, mode)
	})
	return id, err
}

func removePythonPreparation(dir string) error {
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(path, 0700)
		}
		return nil
	}); err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

func (p *Python) prepared(id, digest string) (pythonPreparation, error) {
	var ready pythonPreparation
	if !digestPattern.MatchString(id) || id != p.environmentID(digest) {
		return ready, errors.New("package is not prepared for this source and Python runtime; install or prepare it explicitly")
	}
	dir := filepath.Join(p.root, id)
	info, err := os.Lstat(dir)
	if err != nil {
		return ready, err
	}
	if !info.IsDir() {
		return ready, errors.New("invalid prepared package directory")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "ready.json"))
	if err != nil {
		return ready, err
	}
	if json.Unmarshal(raw, &ready) != nil || ready.ID != id || ready.Digest != digest || ready.Python != p.identity || ready.Version != PythonRuntime {
		return ready, errors.New("invalid prepared package identity")
	}
	return ready, nil
}

func (p *Python) Ready(id, digest string) error { _, err := p.prepared(id, digest); return err }

// Ensure returns this process's environment for an approved package digest,
// preparing it from the package files when it is missing. An environment is a
// local copy of the approved files for the configured Python and embedded client,
// so another gateway, a restart on ephemeral storage or a changed interpreter
// prepares the same approved files again. Prepare rechecks the digest.
func (p *Python) Ensure(ctx context.Context, files map[string]string, digest string) (string, error) {
	id := p.environmentID(digest)
	if _, err := p.prepared(id, digest); err == nil {
		return id, nil
	}
	return p.Prepare(ctx, files, digest)
}

type pythonDiagnostic struct {
	mu   sync.Mutex
	text []byte
}

func (b *pythonDiagnostic) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.text = append(b.text, data...)
	if len(b.text) > 8192 {
		b.text = b.text[len(b.text)-8192:]
	}
	return len(data), nil
}
func (b *pythonDiagnostic) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	text := strings.TrimSpace(string(b.text))
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		key = strings.ToUpper(key)
		if len(value) >= 8 && (strings.Contains(key, "TOKEN") || strings.Contains(key, "SECRET") || strings.Contains(key, "PASSWORD") || strings.Contains(key, "KEY") || strings.Contains(key, "CREDENTIAL")) {
			text = strings.ReplaceAll(text, value, "[redacted]")
		}
	}
	return text
}

func runPythonPreparation(ctx context.Context, executable, dir string, args ...string) error {
	cmd := pythonCommand(ctx, executable, dir, args...)
	var diagnostic pythonDiagnostic
	cmd.Stdout = &diagnostic
	cmd.Stderr = &diagnostic
	err := cmd.Run()
	if cmd.Process != nil {
		stopPythonGroup(cmd.Process.Pid)
	}
	if err != nil {
		return fmt.Errorf("Python preparation: %w: %s", errors.Join(err, ctx.Err()), diagnostic.String())
	}
	return nil
}

func stopPythonGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	// When the server is container PID 1 it adopts orphaned descendants. Reap
	// only this run's group, never another Go command's children. Outside PID 1,
	// the OS reaper owns those descendants and Wait4 simply returns ECHILD.
	for {
		_, err := syscall.Wait4(-pid, nil, 0, nil)
		if err != nil && !errors.Is(err, syscall.EINTR) {
			return
		}
	}
}

// Run uses an already-prepared version. It never installs or resolves libraries.
// The pipes are the run's authority: there is no callback URL or bearer token.
// A sandboxed run is refused rather than started without its confinement.
func (p *Python) Run(ctx context.Context, id, digest string, sandboxed bool, call func([]byte) (any, error)) error {
	ready, err := p.prepared(id, digest)
	if err != nil {
		return err
	}
	current, err := statInterpreter(p.executable)
	if err != nil {
		return err
	}
	if current != p.interpreter {
		return errors.New("configured Python changed; prepare a new version after restarting the server")
	}
	dir := filepath.Join(p.root, id)
	raw, err := os.ReadFile(filepath.Join(dir, "source", "automation.json"))
	if err != nil {
		return err
	}
	m, err := ParseManifest(raw)
	if err != nil {
		return err
	}
	files := map[string]string{"automation.json": string(raw)}
	for _, name := range m.Files {
		b, err := os.ReadFile(filepath.Join(dir, "source", name))
		if err != nil {
			return err
		}
		files[name] = string(b)
	}
	if PackageDigest(files) != digest || m.Entrypoint != ready.Entrypoint {
		return errors.New("prepared package files changed")
	}
	scratch, err := os.MkdirTemp(p.root, "run-")
	if err != nil {
		return err
	}
	defer removePythonPreparation(scratch)
	bin := p.executable
	args := []string{"-I", "-S", "-B"}
	if ready.Requirements != "" {
		bin = filepath.Join(dir, "venv", "bin", "python")
		args = []string{"-I", "-B"}
	}
	args = append(args, filepath.Join(dir, "client", "runner.py"), filepath.Join(dir, "source"), m.Entrypoint)
	parentCtx := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := pythonCommand(ctx, bin, scratch, args...)
	if sandboxed {
		if err = p.confine(cmd, dir, scratch); err != nil {
			return err
		}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer stdin.Close()
	output, childOutput, err := os.Pipe()
	if err != nil {
		return err
	}
	defer output.Close()
	defer childOutput.Close()
	life, alive, err := os.Pipe()
	if err != nil {
		return err
	}
	defer life.Close()
	defer alive.Close()
	cmd.Stdout = childOutput
	cmd.ExtraFiles = []*os.File{life}
	var diagnostic pythonDiagnostic
	cmd.Stderr = &diagnostic
	if err = cmd.Start(); err != nil {
		return fmt.Errorf("start Python package: %w", err)
	}
	childOutput.Close()
	life.Close()
	done := make(chan error, 1)
	go func() { err := cmd.Wait(); stopPythonGroup(cmd.Process.Pid); done <- err }()
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 4096), capacity.StateBytes.Limit())
	var protocolErr error
	for scanner.Scan() {
		result, err := call(append([]byte{}, scanner.Bytes()...))
		if errors.Is(err, ErrPythonProtocol) {
			protocolErr = err
			break
		}
		answer := map[string]any{"result": result}
		if err != nil {
			answer = map[string]any{"error": err.Error()}
		}
		raw, encodeErr := json.Marshal(answer)
		if encodeErr != nil {
			protocolErr = encodeErr
			break
		}
		if len(raw) > capacity.StateBytes.Limit() {
			protocolErr = capacity.StateBytes.Error()
			break
		}
		if _, writeErr := stdin.Write(append(raw, '\n')); writeErr != nil {
			protocolErr = writeErr
			break
		}
	}
	if err := scanner.Err(); err != nil {
		protocolErr = fmt.Errorf("Python protocol: %w", err)
	}
	if protocolErr != nil {
		cancel()
	}
	stdin.Close()
	waitErr := <-done
	if protocolErr != nil || waitErr != nil || parentCtx.Err() != nil {
		detail := diagnostic.String()
		if sandboxed && strings.Contains(detail, sandbox.NoticePrefix) {
			detail += "; " + sandboxOptOut
		}
		return fmt.Errorf("Python package: %w: %s", errors.Join(protocolErr, waitErr, parentCtx.Err()), detail)
	}
	return nil
}

// sandboxOptOut ends each refusal of a sandboxed run.
const sandboxOptOut = `to run this installation as unconfined server code, set "sandbox": false in its script (clear Sandbox package code in Administration)`

// confine re-executes a run through the strict sandbox helper, which keeps its
// PID, process group, environment and pipes. The prepared version (source,
// runner and any venv) and the interpreter's installation are read-only; the
// run's scratch directory is its only writable data. No grant may contain the
// prepared root, and so DATA_DIR with its notebooks, catalog and credentials.
// The helper refuses to run without Landlock and denies every socket, since the
// host contract needs only the pipes; checking Landlock here first names the
// opt-out before any process starts. The process limit is a fork-bomb guard;
// deadlines and the container still bound CPU and memory.
func (p *Python) confine(cmd *exec.Cmd, version, scratch string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err = sandbox.Available(); err != nil {
		return fmt.Errorf("package sandbox refused the run: %w; %s", err, sandboxOptOut)
	}
	if !filepath.IsAbs(p.prefix) {
		return fmt.Errorf("configured Python reports no absolute installation prefix to sandbox; %s", sandboxOptOut)
	}
	policy := sandbox.Policy{Processes: 4096, Strict: true, Read: []string{version, p.prefix}, Write: []string{scratch}, Private: []string{p.root}}
	cmd.Args = append([]string{self}, policy.Args(cmd.Path, cmd.Args[1:]...)...)
	cmd.Path = self
	return nil
}

// DecodeScriptRequest rejects non-protocol stdout without copying its potentially
// sensitive contents into diagnostics. The same typed input feeds every host call.
func DecodeScriptRequest(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return fmt.Errorf("%w (expected one JSON object)", ErrPythonProtocol)
	}
	if d.Decode(new(any)) != io.EOF {
		return fmt.Errorf("%w (trailing data)", ErrPythonProtocol)
	}
	return nil
}
