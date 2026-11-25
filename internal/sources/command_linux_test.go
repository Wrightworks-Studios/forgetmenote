//go:build linux

package sources

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// A compromised parser must not reach instance data such as oauth.json: only its
// scratch directory (which holds the input) is readable or writable.
func TestLocalExtractorsCannotReachDataOutsideScratch(t *testing.T) {
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION); errno != 0 {
		t.Skip("kernel does not provide Landlock:", errno)
	}
	data := t.TempDir()
	secret := filepath.Join(data, "oauth.json")
	if err := os.WriteFile(secret, []byte("leaked-token"), 0600); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(data, "planted")
	stubPDFText(t, "secret-$(cat '"+secret+"' 2>/dev/null)-input-$(head -c 4 source.pdf)-$(echo x > '"+planted+"' 2>/dev/null && echo wrote)")
	got, err := Prepare(t.Context(), "probe.pdf", "", []byte("%PDF-1.4\nfixture"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Text, "leaked-token") || strings.Contains(got.Text, "wrote") || !strings.Contains(got.Text, "input-%PDF") {
		t.Fatalf("extractor escaped its scratch directory: %q", got.Text)
	}
	if _, err := os.Stat(planted); !os.IsNotExist(err) {
		t.Fatal("extractor wrote outside scratch", err)
	}
}
