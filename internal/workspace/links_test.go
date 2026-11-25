package workspace

import (
	"strings"
	"testing"
)

func TestMoveLinksKeepCodeAndHandleEscapedDestinations(t *testing.T) {
	source := []byte("[A](old/a(test).md#part)\n\n[A](<old/a b.md>)\n\n````md\n```\n[A](old/a(test).md)\n````\n\n<!-- [A](old/a(test).md) -->\n")
	got := string(rewriteLinks(source, "index.md", "index.md", "old", "new"))
	for _, want := range []string{"[A](new/a%28test%29.md#part)", "[A](<new/a%20b.md>)", "````md\n```\n[A](old/a(test).md)\n````", "<!-- [A](old/a(test).md) -->"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %s", want, got)
		}
	}
}
