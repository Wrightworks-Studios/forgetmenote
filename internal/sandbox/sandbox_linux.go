//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"io"
	"unsafe"

	"golang.org/x/net/bpf"
	"golang.org/x/sys/unix"
)

// RLIMIT_DATA bounds heap and private writable mappings (Linux 4.7 or newer)
// without counting reserved address space, which emulators such as Rosetta
// map far beyond the allocation budget.
const memoryLimit = true

const (
	abi1Access = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_DIR | unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK | unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM
	readAccess  = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR
	writeAccess = unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE | unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG | unix.LANDLOCK_ACCESS_FS_TRUNCATE | unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	// Rights that apply to a regular file or device rather than a directory.
	fileAccess = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_TRUNCATE | unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
)

// Available reports why a Strict policy cannot run here, or nil when it can.
func Available() error {
	if _, err := landlockABI(); err != nil {
		return fmt.Errorf("Landlock unavailable: %w", err)
	}
	return nil
}

// landlockABI reports the kernel's Landlock ABI version. ENOSYS (no syscall),
// EOPNOTSUPP (disabled at boot) and EPERM (a seccomp profile denies it) all mean
// the kernel cannot confine this tool.
func landlockABI() (int, error) {
	abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return 0, errno
	}
	return int(abi), nil
}

// restrict sets no_new_privs and, where available, a Landlock domain that
// handles every access right this ABI knows. Only the listed paths are granted;
// symlink, device, FIFO and socket creation, cross-directory links and renames,
// TCP bind/connect (ABI 4+), abstract Unix sockets and signals to processes
// outside the sandbox (ABI 6+) are denied. A strict domain must exist, and
// strictFilter then also applies.
func restrict(rules []rule, strict bool, notice io.Writer) error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no_new_privs: %w", err)
	}
	abi, err := landlockABI()
	if err != nil && strict {
		return fmt.Errorf("Landlock unavailable (%w); refusing to run without the sandbox", err)
	}
	if err != nil {
		fmt.Fprintf(notice, "%sLandlock unavailable (%v); continuing with resource limits only\n", NoticePrefix, err)
		return nil
	}
	handled := uint64(abi1Access)
	if abi >= 2 {
		handled |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		handled |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	if abi >= 5 {
		handled |= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}
	attr := unix.LandlockRulesetAttr{Handled_access_fs: handled}
	if abi >= 4 {
		attr.Handled_access_net = unix.LANDLOCK_ACCESS_NET_BIND_TCP | unix.LANDLOCK_ACCESS_NET_CONNECT_TCP
	} else if !strict {
		fmt.Fprintf(notice, "%sLandlock ABI %d cannot restrict TCP; filesystem rules still apply\n", NoticePrefix, abi)
	}
	if abi >= 6 {
		attr.Scoped = unix.LANDLOCK_SCOPE_ABSTRACT_UNIX_SOCKET | unix.LANDLOCK_SCOPE_SIGNAL
	}
	ruleset, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return fmt.Errorf("create Landlock ruleset: %w", errno)
	}
	defer unix.Close(int(ruleset))
	for _, r := range rules {
		if err := addRule(int(ruleset), r, handled); err != nil {
			return err
		}
	}
	if _, _, errno = unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, ruleset, 0, 0); errno != 0 {
		return fmt.Errorf("enforce Landlock ruleset: %w", errno)
	}
	if strict {
		return installStrictFilter()
	}
	return nil
}

// strictFilter is a seccomp program over struct seccomp_data. socket(2) and
// io_uring_setup(2) (whose rings can open sockets) fail with EACCES. socketpair
// creates only stream or sequenced-packet pairs: a datagram pair could sendto
// any pathname socket. Path truncate(2), which Landlock handles only from ABI 3,
// is denied too; ftruncate on files the tool opened for writing still works.
// It decodes both supported system call tables whatever GOARCH built it,
// because emulators present the host's; any other architecture, including
// 32-bit compatibility calls, gets ENOSYS.
var strictFilter = []bpf.Instruction{
	bpf.LoadAbsolute{Off: 4, Size: 4}, // arch
	bpf.JumpIf{Cond: bpf.JumpEqual, Val: unix.AUDIT_ARCH_X86_64, SkipFalse: 5},
	bpf.LoadAbsolute{Off: 0, Size: 4},                                       // x86-64 system call number
	bpf.JumpIf{Cond: bpf.JumpGreaterOrEqual, Val: 0x40000000, SkipTrue: 13}, // any x32 call
	bpf.JumpIf{Cond: bpf.JumpEqual, Val: 41, SkipTrue: 12},                  // socket
	bpf.JumpIf{Cond: bpf.JumpEqual, Val: 76, SkipTrue: 11},                  // truncate
	bpf.JumpIf{Cond: bpf.JumpEqual, Val: 53, SkipTrue: 6, SkipFalse: 5},     // socketpair
	bpf.JumpIf{Cond: bpf.JumpEqual, Val: unix.AUDIT_ARCH_AARCH64, SkipFalse: 11},
	bpf.LoadAbsolute{Off: 0, Size: 4},                                    // arm64 system call number
	bpf.JumpIf{Cond: bpf.JumpEqual, Val: 198, SkipTrue: 7},               // socket
	bpf.JumpIf{Cond: bpf.JumpEqual, Val: 45, SkipTrue: 6},                // truncate
	bpf.JumpIf{Cond: bpf.JumpEqual, Val: 199, SkipTrue: 1},               // socketpair
	bpf.JumpIf{Cond: bpf.JumpEqual, Val: 425, SkipTrue: 4, SkipFalse: 5}, // io_uring_setup
	bpf.LoadAbsolute{Off: 24, Size: 4},                                   // low word of the type argument
	bpf.ALUOpConstant{Op: bpf.ALUOpAnd, Val: 0xf},                        // without SOCK_CLOEXEC and SOCK_NONBLOCK
	bpf.JumpIf{Cond: bpf.JumpEqual, Val: unix.SOCK_STREAM, SkipTrue: 2},
	bpf.JumpIf{Cond: bpf.JumpEqual, Val: unix.SOCK_SEQPACKET, SkipTrue: 1},
	bpf.RetConstant{Val: unix.SECCOMP_RET_ERRNO | uint32(unix.EACCES)},
	bpf.RetConstant{Val: unix.SECCOMP_RET_ALLOW},
	bpf.RetConstant{Val: unix.SECCOMP_RET_ERRNO | uint32(unix.ENOSYS)},
}

// installStrictFilter applies strictFilter to this thread, which then execs the tool.
func installStrictFilter() error {
	raw, err := bpf.Assemble(strictFilter)
	if err != nil {
		return fmt.Errorf("assemble seccomp filter: %w", err)
	}
	program := make([]unix.SockFilter, len(raw))
	for i, ins := range raw {
		program[i] = unix.SockFilter{Code: ins.Op, Jt: ins.Jt, Jf: ins.Jf, K: ins.K}
	}
	prog := unix.SockFprog{Len: uint16(len(program)), Filter: &program[0]}
	if err = unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&prog)), 0, 0); err != nil {
		return fmt.Errorf("install seccomp filter: %w", err)
	}
	return nil
}

func addRule(ruleset int, r rule, handled uint64) error {
	fd, err := unix.Open(r.path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) && !r.required {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open sandbox path %s: %w", r.path, err)
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("inspect sandbox path %s: %w", r.path, err)
	}
	access := uint64(readAccess)
	if r.write {
		access = writeAccess
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		access &= fileAccess
	}
	beneath := unix.LandlockPathBeneathAttr{Allowed_access: access & handled, Parent_fd: int32(fd)}
	if _, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(ruleset), unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&beneath)), 0, 0, 0); errno != 0 {
		return fmt.Errorf("allow sandbox path %s: %w", r.path, errno)
	}
	return nil
}
