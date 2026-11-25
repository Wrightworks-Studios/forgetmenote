package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The server re-executes itself as the extractor sandbox; the subcommand must be
// handled before the ordinary command-line parser or server startup.
func TestSandboxExecRunsToolUnderLimits(t *testing.T) {
	cmd := exec.Command(os.Args[0], "sandbox-exec", "-cpu=9", "--", "sh", "-c", "ulimit -t; ulimit -c")
	var diagnostics strings.Builder
	cmd.Stderr = &diagnostics
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err, diagnostics.String())
	}
	limits := strings.Fields(string(out))
	if len(limits) != 2 || limits[0] != "9" || limits[1] != "0" {
		t.Fatalf("sandbox-exec did not apply limits before running the tool: %q", out)
	}
}
