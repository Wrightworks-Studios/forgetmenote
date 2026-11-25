package webcore

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateRenderCorpus = flag.Bool("update-render-corpus", false, "rewrite testdata/render-corpus/*.html from the renderer")

// The render corpus pins what Read shows for each Markdown construct. The
// browser renders private drafts and Side by side's pane itself, and
// internal/web/ui/rendered-preview.test.mjs compares that renderer with these
// goldens, so a change on either side shows up as a failing test.
func TestRenderCorpusGolden(t *testing.T) {
	sources, err := filepath.Glob(filepath.Join("testdata", "render-corpus", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) == 0 {
		t.Fatal("empty render corpus")
	}
	renderer := NewRenderer()
	for _, source := range sources {
		t.Run(filepath.Base(source), func(t *testing.T) {
			markdown, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			html := renderer.Render(string(markdown))
			golden := strings.TrimSuffix(source, ".md") + ".html"
			if *updateRenderCorpus {
				if err := os.WriteFile(golden, []byte(html), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run with -update-render-corpus to create it)", err)
			}
			if html != string(want) {
				t.Fatalf("%s renders differently from its golden (run with -update-render-corpus after checking the change)\ngot:\n%s\nwant:\n%s", source, html, want)
			}
		})
	}
}
