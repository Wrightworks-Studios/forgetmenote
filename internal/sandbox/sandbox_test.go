package sandbox

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPolicyArgumentsRoundTrip(t *testing.T) {
	policy := Policy{Memory: 1 << 30, CPU: 50, Processes: 4096, FileSize: 64 << 20, Strict: true, Read: []string{"/inputs/source.pdf"}, Write: []string{"/scratch/job", "/scratch/other"}, Private: []string{"/data"}}
	args := policy.Args("pdftotext", "-enc", "UTF-8", "-", "--")
	if args[0] != Command {
		t.Fatal("helper subcommand missing", args)
	}
	parsed, argv, err := Parse(args[1:])
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed, policy) || !reflect.DeepEqual(argv, []string{"pdftotext", "-enc", "UTF-8", "-", "--"}) {
		t.Fatalf("round trip changed policy or tool arguments: %+v %q", parsed, argv)
	}
	for _, bad := range [][]string{{}, {"--"}, {"-memory=lots", "--", "tool"}, {"-unknown", "--", "tool"}, {"-write=relative", "--", "tool"}, {"-private=relative", "--", "tool"}} {
		if _, _, err := Parse(bad); err == nil {
			t.Fatalf("accepted invalid helper arguments %q", bad)
		}
	}
}

// The helper process reports its own limits; it is the sandboxed "tool".
func TestHelperReportLimits(t *testing.T) {
	if os.Getenv("FMN_SANDBOX_HELPER") != "limits" {
		t.Skip("subprocess helper")
	}
	out := map[string][2]uint64{}
	for name, resource := range map[string]int{"cpu": unix.RLIMIT_CPU, "fsize": unix.RLIMIT_FSIZE, "core": unix.RLIMIT_CORE, "nproc": unix.RLIMIT_NPROC, "data": unix.RLIMIT_DATA, "as": unix.RLIMIT_AS} {
		var limit unix.Rlimit
		if err := unix.Getrlimit(resource, &limit); err != nil {
			t.Fatal(err)
		}
		out[name] = [2]uint64{limit.Cur, limit.Max}
	}
	raw, _ := json.Marshal(out)
	os.Stdout.Write(append(append([]byte("limits:"), raw...), '\n'))
}

func TestSandboxLowersResourceLimitsBeforeExec(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var processes unix.Rlimit
	if err = unix.Getrlimit(unix.RLIMIT_NPROC, &processes); err != nil {
		t.Fatal(err)
	}
	policy := Policy{CPU: 7, FileSize: 1 << 20, Processes: min(processes.Max, 3000), Memory: 8 << 30}
	cmd := exec.Command(self, policy.Args(self, "-test.run=^TestHelperReportLimits$")...)
	cmd.Env = append(os.Environ(), "FMN_SANDBOX_HELPER=limits")
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(err, string(raw))
	}
	_, report, found := strings.Cut(string(raw), "limits:")
	if !found {
		t.Fatal("helper did not run under the sandbox", string(raw))
	}
	var got map[string][2]uint64
	if err = json.Unmarshal([]byte(strings.SplitN(report, "\n", 2)[0]), &got); err != nil {
		t.Fatal(err, string(raw))
	}
	want := map[string]uint64{"cpu": 7, "fsize": 1 << 20, "core": 0, "nproc": policy.Processes}
	if runtime.GOOS == "linux" {
		// The memory limit bounds allocations, not reserved address space,
		// which emulators such as Rosetta need far beyond the allocation budget.
		want["data"] = 8 << 30
		if got["as"] == [2]uint64{8 << 30, 8 << 30} {
			t.Fatalf("memory limit lowered the address space: %v", got)
		}
	}
	for name, value := range want {
		if got[name] != [2]uint64{value, value} {
			t.Fatalf("%s limit = %v, want soft and hard %d (all %v)", name, got[name], value, got)
		}
	}
}

func TestSandboxReportsMissingTool(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, Policy{}.Args("forgetmenote-missing-sandbox-tool")...)
	cmd.Env = []string{"PATH=" + t.TempDir()}
	raw, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 127 || !strings.Contains(string(raw), NoticePrefix) {
		t.Fatalf("missing tool was not reported: %v %q", err, raw)
	}
}

// A grant that contains instance data, such as a binary installed in the parent
// of its data directory, must stop the tool before it runs instead of exposing
// that data. Grants beneath the private path, such as a run's scratch, are fine.
func TestSandboxRefusesGrantsContainingPrivatePaths(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	data, scratch := filepath.Join(root, "data"), filepath.Join(root, "data", "run")
	if err = os.MkdirAll(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command(self, Policy{Read: []string{root}, Private: []string{data}}.Args("sh", "-c", "echo tool ran")...).CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 126 || strings.Contains(string(raw), "tool ran") || !strings.Contains(string(raw), NoticePrefix) {
		t.Fatalf("grant containing a private path was not refused: %v %q", err, raw)
	}
	raw, err = exec.Command(self, Policy{Write: []string{scratch}, Private: []string{data}}.Args("sh", "-c", "echo tool ran")...).CombinedOutput()
	if err != nil || !strings.Contains(string(raw), "tool ran") {
		t.Fatalf("grant beneath a private path was refused: %v %q", err, raw)
	}
}

// Strict callers rely on the confinement; without Landlock the tool never runs.
func TestStrictSandboxRefusesWithoutLandlock(t *testing.T) {
	if Available() == nil {
		t.Skip("this kernel enforces the strict sandbox")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command(self, Policy{Strict: true}.Args("sh", "-c", "echo tool ran")...).CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 126 || strings.Contains(string(raw), "tool ran") || !strings.Contains(string(raw), "Landlock") {
		t.Fatalf("strict sandbox ran a tool without Landlock: %v %q", err, raw)
	}
}
