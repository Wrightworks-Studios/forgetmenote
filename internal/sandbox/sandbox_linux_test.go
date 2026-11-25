//go:build linux

package sandbox

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/net/bpf"
	"golang.org/x/sys/unix"
)

// The helper is the sandboxed tool: it reports which accesses the kernel allowed.
func TestHelperProbeAccess(t *testing.T) {
	if os.Getenv("FMN_SANDBOX_HELPER") != "access" {
		t.Skip("subprocess helper")
	}
	scratch, outside := os.Getenv("FMN_SANDBOX_SCRATCH"), os.Getenv("FMN_SANDBOX_OUTSIDE")
	result := func(name string, err error) {
		state := "allowed"
		if err != nil {
			state = "denied"
		}
		fmt.Printf("%s=%s\n", name, state)
	}
	_, err := os.ReadFile(filepath.Join(outside, "secret"))
	result("read-outside", err)
	_, err = os.ReadFile(os.Getenv("FMN_SANDBOX_INPUT"))
	result("read-input", err)
	result("write-scratch", os.WriteFile(filepath.Join(scratch, "output"), []byte("ok"), 0600))
	result("write-outside", os.WriteFile(filepath.Join(outside, "planted"), []byte("no"), 0600))
	result("symlink-scratch", os.Symlink(filepath.Join(outside, "secret"), filepath.Join(scratch, "link")))
	result("truncate-outside", unix.Truncate(filepath.Join(outside, "secret"), 0))
	for _, probe := range []struct{ name, network, address string }{{"tcp-connect", "tcp", os.Getenv("FMN_SANDBOX_TCP")}, {"udp-send", "udp", os.Getenv("FMN_SANDBOX_UDP")}, {"unix-connect", "unix", os.Getenv("FMN_SANDBOX_UNIX")}} {
		conn, err := net.Dial(probe.network, probe.address)
		if err == nil {
			_, err = conn.Write([]byte("x"))
			conn.Close()
		}
		result(probe.name, err)
	}
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err == nil {
		unix.Close(pair[0])
		unix.Close(pair[1])
	}
	result("stream-pair", err)
	// A datagram pair can address any pathname socket with sendto.
	pair, err = unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err == nil {
		err = unix.Sendto(pair[0], []byte("x"), 0, &unix.SockaddrUnix{Name: os.Getenv("FMN_SANDBOX_UNIXGRAM")})
		unix.Close(pair[0])
		unix.Close(pair[1])
	}
	result("datagram-pair", err)
}

type probeDirs struct{ root, scratch, outside, input string }

func probeSandbox(t *testing.T, policy func(probeDirs) Policy) (string, probeDirs) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dirs := probeDirs{root: root, scratch: filepath.Join(root, "scratch"), outside: filepath.Join(root, "outside")}
	inputs := filepath.Join(root, "inputs")
	for _, dir := range []string{dirs.scratch, dirs.outside, inputs} {
		if err = os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	dirs.input = filepath.Join(inputs, "source.pdf")
	if err = os.WriteFile(dirs.input, []byte("%PDF"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dirs.outside, "secret"), []byte("oauth"), 0600); err != nil {
		t.Fatal(err)
	}
	// The helper may read its own executable's directory.
	if rel, err := filepath.Rel(filepath.Dir(self), root); err == nil && !strings.HasPrefix(rel, "..") {
		t.Fatal("place the test binary outside the temporary directory it probes", self, root)
	}
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
	stream, err := net.Listen("unix", filepath.Join(dirs.outside, "stream.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	datagram, err := net.ListenPacket("unixgram", filepath.Join(dirs.outside, "datagram.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer datagram.Close()
	cmd := exec.Command(self, policy(dirs).Args(self, "-test.run=^TestHelperProbeAccess$")...)
	cmd.Env = append(os.Environ(), "FMN_SANDBOX_HELPER=access", "FMN_SANDBOX_SCRATCH="+dirs.scratch, "FMN_SANDBOX_OUTSIDE="+dirs.outside, "FMN_SANDBOX_INPUT="+dirs.input,
		"FMN_SANDBOX_TCP="+tcp.Addr().String(), "FMN_SANDBOX_UDP="+udp.LocalAddr().String(), "FMN_SANDBOX_UNIX="+stream.Addr().String(), "FMN_SANDBOX_UNIXGRAM="+datagram.LocalAddr().String())
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(err, string(raw))
	}
	return string(raw), dirs
}

func expectProbes(t *testing.T, raw string, want map[string]string) {
	t.Helper()
	abi, _ := landlockABI()
	for name, state := range want {
		if !strings.Contains(raw, name+"="+state+"\n") {
			t.Fatalf("Landlock ABI %d: %s not %s:\n%s", abi, name, state, raw)
		}
	}
}

func TestSandboxConfinesFilesAndNetwork(t *testing.T) {
	abi, err := landlockABI()
	if err != nil {
		t.Skip("kernel does not provide Landlock:", err)
	}
	raw, dirs := probeSandbox(t, func(d probeDirs) Policy {
		return Policy{Memory: 8 << 30, CPU: 30, FileSize: 1 << 20, Read: []string{d.input}, Write: []string{d.scratch}}
	})
	want := map[string]string{"read-outside": "denied", "read-input": "allowed", "write-scratch": "allowed", "write-outside": "denied", "symlink-scratch": "denied", "stream-pair": "allowed"}
	if abi >= 4 {
		want["tcp-connect"] = "denied"
	}
	expectProbes(t, raw, want)
	if _, err = os.Stat(filepath.Join(dirs.outside, "planted")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("sandboxed tool wrote outside its scratch directory", err)
	}
	if abi >= 4 && strings.Contains(raw, NoticePrefix) {
		t.Fatal("full sandbox reported a degraded notice", raw)
	}
}

// Strict mode denies every socket a tool could open and path truncation (which
// Landlock handles only from ABI 3), on every Landlock ABI, while in-process
// stream pairs (used by Python's asyncio) keep working.
func TestStrictSandboxDeniesEverySocket(t *testing.T) {
	if err := Available(); err != nil {
		t.Skip(err)
	}
	raw, _ := probeSandbox(t, func(d probeDirs) Policy {
		return Policy{Strict: true, Read: []string{d.input}, Write: []string{d.scratch}, Private: []string{d.outside}}
	})
	expectProbes(t, raw, map[string]string{"read-outside": "denied", "read-input": "allowed", "write-scratch": "allowed", "write-outside": "denied",
		"truncate-outside": "denied", "tcp-connect": "denied", "udp-send": "denied", "unix-connect": "denied", "datagram-pair": "denied", "stream-pair": "allowed"})
	if strings.Contains(raw, NoticePrefix) {
		t.Fatal("strict sandbox reported a degraded notice", raw)
	}
}

// Emulators such as Rosetta translate system calls to the host architecture, so
// the filter decodes both supported tables whatever GOARCH built it, and refuses
// every other architecture, including 32-bit compatibility calls.
func TestStrictFilterDecodesEachArchitecture(t *testing.T) {
	// Run the encoding the kernel receives, not only the source instructions.
	raw, err := bpf.Assemble(strictFilter)
	if err != nil {
		t.Fatal(err)
	}
	program, decoded := bpf.Disassemble(raw)
	vm, err := bpf.NewVM(program)
	if err != nil || !decoded {
		t.Fatal("seccomp filter does not round-trip", err)
	}
	deny, allow, foreign := uint32(unix.SECCOMP_RET_ERRNO|unix.EACCES), uint32(unix.SECCOMP_RET_ALLOW), uint32(unix.SECCOMP_RET_ERRNO|unix.ENOSYS)
	for _, tc := range []struct {
		name          string
		arch, nr, typ uint32
		want          uint32
	}{
		{"x86-64 socket", unix.AUDIT_ARCH_X86_64, 41, 0, deny},
		{"x86-64 x32 socket", unix.AUDIT_ARCH_X86_64, 41 | 0x40000000, 0, deny},
		{"x86-64 stream pair", unix.AUDIT_ARCH_X86_64, 53, unix.SOCK_STREAM | unix.SOCK_CLOEXEC, allow},
		{"x86-64 sequenced pair", unix.AUDIT_ARCH_X86_64, 53, unix.SOCK_SEQPACKET, allow},
		{"x86-64 datagram pair", unix.AUDIT_ARCH_X86_64, 53, unix.SOCK_DGRAM | unix.SOCK_NONBLOCK, deny},
		{"x86-64 io_uring", unix.AUDIT_ARCH_X86_64, 425, 0, deny},
		{"x86-64 truncate", unix.AUDIT_ARCH_X86_64, 76, 0, deny},
		{"x86-64 ftruncate", unix.AUDIT_ARCH_X86_64, 77, 0, allow},
		{"x86-64 openat", unix.AUDIT_ARCH_X86_64, 257, 0, allow},
		{"x86-64 arm64 socket number", unix.AUDIT_ARCH_X86_64, 198, 0, allow},
		{"arm64 socket", unix.AUDIT_ARCH_AARCH64, 198, 0, deny},
		{"arm64 stream pair", unix.AUDIT_ARCH_AARCH64, 199, unix.SOCK_STREAM, allow},
		{"arm64 raw pair", unix.AUDIT_ARCH_AARCH64, 199, unix.SOCK_RAW, deny},
		{"arm64 io_uring", unix.AUDIT_ARCH_AARCH64, 425, 0, deny},
		{"arm64 truncate", unix.AUDIT_ARCH_AARCH64, 45, 0, deny},
		{"arm64 ftruncate", unix.AUDIT_ARCH_AARCH64, 46, 0, allow},
		{"arm64 openat", unix.AUDIT_ARCH_AARCH64, 56, 0, allow},
		{"arm64 x86-64 socket number", unix.AUDIT_ARCH_AARCH64, 41, 0, allow},
		{"i386 socketcall", unix.AUDIT_ARCH_I386, 102, 0, foreign},
		{"arm socket", unix.AUDIT_ARCH_ARM, 281, 0, foreign},
	} {
		// struct seccomp_data: nr, arch, instruction pointer, then six 64-bit
		// arguments. The VM loads big-endian words; the kernel loads native
		// little-endian ones, where a 64-bit argument's low word comes first.
		data := make([]byte, 64)
		binary.BigEndian.PutUint32(data[0:], tc.nr)
		binary.BigEndian.PutUint32(data[4:], tc.arch)
		binary.BigEndian.PutUint32(data[24:], tc.typ)
		got, err := vm.Run(data)
		if err != nil || uint32(got) != tc.want {
			t.Errorf("%s: filter returned %#x (%v), want %#x", tc.name, got, err, tc.want)
		}
	}
}
