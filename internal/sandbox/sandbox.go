// Package sandbox runs one local tool on untrusted input with lowered resource
// limits and, on Linux, a Landlock filesystem and network ruleset. The server
// re-executes its own binary as "forgetmenote sandbox-exec ... -- tool args";
// the helper restricts itself and then replaces itself with the tool, so the
// tool keeps the caller's PID, process group, environment and pipes.
//
// Landlock is best effort by default. When the kernel or a seccomp profile does
// not provide it, the helper writes one NoticePrefix line to stderr and continues
// with the resource limits, which apply regardless. A Strict policy instead
// refuses to run the tool, and also denies it sockets and path truncation.
package sandbox

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Command is the hidden subcommand. It is dispatched from this package's init,
// before main, so the server binary and every test binary that links an
// extractor handle it identically instead of recursing into their own main.
const Command = "sandbox-exec"

// NoticePrefix starts every line the helper itself writes to stderr.
const NoticePrefix = "forgetmenote sandbox: "

// Policy is the complete per-invocation sandbox. Zero limits leave that limit
// unchanged; the core-dump limit is always zero. Limits only ever decrease.
type Policy struct {
	Memory    uint64   // RLIMIT_DATA bytes: heap and private writable mappings (Linux only)
	CPU       uint64   // RLIMIT_CPU seconds
	Processes uint64   // RLIMIT_NPROC: tasks of the real user ID, not just this tool
	FileSize  uint64   // RLIMIT_FSIZE bytes per written file
	Read      []string // additional absolute read-only data paths, such as the input file
	Write     []string // absolute read-write paths, such as the per-job scratch directory
	Private   []string // absolute paths no grant may contain; such a grant stops the tool
	// Strict requires Landlock and adds a seccomp filter that denies creating
	// sockets (TCP, UDP and Unix sockets, which Landlock covers only partly, if
	// at all) and truncating files by path, which Landlock ABI 1 and 2 allow.
	// Pipes and connected stream pairs within the tool still work.
	Strict bool
}

// System paths a dynamically linked tool needs: programs, libraries, locale and
// configuration data. Missing entries (for example /lib64 on arm64) are skipped.
// The fontconfig cache spares Poppler a full font scan. CPU topology lets native
// libraries such as ONNX Runtime and OpenMP detect cores and processor features.
var systemRead = []string{"/usr", "/lib", "/lib64", "/bin", "/sbin", "/etc", "/var/cache/fontconfig", "/dev/urandom", "/sys/devices/system/cpu", "/proc/cpuinfo"}
var systemWrite = []string{os.DevNull}

func init() {
	if len(os.Args) > 1 && os.Args[1] == Command {
		os.Exit(Main(os.Args[2:], os.Stderr))
	}
}

// Args returns the helper arguments (after the executable) that run tool with args.
func (p Policy) Args(tool string, args ...string) []string {
	out := []string{Command}
	for _, limit := range []struct {
		name  string
		value uint64
	}{{"memory", p.Memory}, {"cpu", p.CPU}, {"processes", p.Processes}, {"file-size", p.FileSize}} {
		if limit.value != 0 {
			out = append(out, "-"+limit.name+"="+strconv.FormatUint(limit.value, 10))
		}
	}
	for _, path := range p.Read {
		out = append(out, "-read="+path)
	}
	for _, path := range p.Write {
		out = append(out, "-write="+path)
	}
	if p.Strict {
		out = append(out, "-strict")
	}
	for _, path := range p.Private {
		out = append(out, "-private="+path)
	}
	return append(append(out, "--", tool), args...)
}

type paths []string

func (p *paths) String() string { return strings.Join(*p, ",") }
func (p *paths) Set(value string) error {
	if !filepath.IsAbs(value) {
		return errors.New("sandbox paths must be absolute")
	}
	*p = append(*p, filepath.Clean(value))
	return nil
}

// Parse reads the helper arguments that follow Command.
func Parse(args []string) (Policy, []string, error) {
	var p Policy
	flags := flag.NewFlagSet(Command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Uint64Var(&p.Memory, "memory", 0, "heap and private writable memory limit in bytes")
	flags.Uint64Var(&p.CPU, "cpu", 0, "CPU limit in seconds")
	flags.Uint64Var(&p.Processes, "processes", 0, "process limit for the real user ID")
	flags.Uint64Var(&p.FileSize, "file-size", 0, "largest written file in bytes")
	flags.Var((*paths)(&p.Read), "read", "read-only path (repeatable)")
	flags.Var((*paths)(&p.Write), "write", "read-write path (repeatable)")
	flags.BoolVar(&p.Strict, "strict", false, "require Landlock and deny sockets")
	flags.Var((*paths)(&p.Private), "private", "path no grant may contain (repeatable)")
	if err := flags.Parse(args); err != nil {
		return p, nil, err
	}
	if flags.NArg() == 0 || flags.Arg(0) == "" {
		return p, nil, errors.New("usage: " + Command + " [limits] [-strict] [-read PATH] [-write PATH] [-private PATH] -- TOOL [ARGS]")
	}
	return p, flags.Args(), nil
}

// Main applies the policy and executes the tool. It returns only on failure:
// 2 for invalid arguments, 127 for a missing tool and 126 otherwise.
func Main(args []string, stderr io.Writer) int {
	// Landlock and no_new_privs bind the calling thread, which must then exec.
	runtime.LockOSThread()
	fail := func(code int, err error) int {
		fmt.Fprintln(stderr, NoticePrefix+err.Error())
		return code
	}
	policy, argv, err := Parse(args)
	if err != nil {
		return fail(2, err)
	}
	tool, err := exec.LookPath(argv[0])
	if err == nil {
		tool, err = filepath.Abs(tool)
	}
	if err != nil {
		return fail(127, err)
	}
	read := []rule{{path: tool, required: true}}
	for _, path := range systemRead {
		read = append(read, rule{path: path})
	}
	if self, err := os.Executable(); err == nil {
		read = append(read, rule{path: filepath.Dir(self)})
	}
	for _, path := range policy.Read {
		read = append(read, rule{path: path, required: true})
	}
	write := []rule{}
	for _, path := range systemWrite {
		write = append(write, rule{path: path, write: true})
	}
	for _, path := range policy.Write {
		write = append(write, rule{path: path, write: true, required: true})
	}
	rules := append(read, write...)
	for _, private := range policy.Private {
		for _, r := range rules {
			if contains(r.path, private) {
				return fail(126, fmt.Errorf("sandbox grant %s would expose %s", r.path, private))
			}
		}
	}
	if err = restrict(rules, policy.Strict, stderr); err != nil {
		return fail(126, err)
	}
	// The memory limit comes last: from here until exec the helper allocates
	// almost nothing, so its own runtime cannot trip the new limit.
	if err = lowerLimits(policy); err != nil {
		return fail(126, err)
	}
	err = syscall.Exec(tool, argv, os.Environ())
	return fail(126, fmt.Errorf("exec %s: %w", argv[0], err))
}

// contains reports whether path is dir or lies beneath it, after resolving
// symbolic links as Landlock does when it opens a rule's path.
func contains(dir, path string) bool {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// rule grants read (and, with write, modification) beneath one path.
type rule struct {
	path            string
	write, required bool
}

type limit struct {
	resource int
	value    uint64
}

func lowerLimits(p Policy) error {
	limits := []limit{{unix.RLIMIT_CORE, 0}, {unix.RLIMIT_FSIZE, p.FileSize}, {unix.RLIMIT_NPROC, p.Processes}, {unix.RLIMIT_CPU, p.CPU}}
	if memoryLimit {
		limits = append(limits, limit{unix.RLIMIT_DATA, p.Memory})
	}
	for i, l := range limits {
		if i > 0 && l.value == 0 {
			continue
		}
		var current unix.Rlimit
		if err := unix.Getrlimit(l.resource, &current); err != nil {
			return fmt.Errorf("read resource limit %d: %w", l.resource, err)
		}
		value := min(l.value, current.Max)
		if err := unix.Setrlimit(l.resource, &unix.Rlimit{Cur: value, Max: value}); err != nil {
			return fmt.Errorf("set resource limit %d: %w", l.resource, err)
		}
	}
	return nil
}
