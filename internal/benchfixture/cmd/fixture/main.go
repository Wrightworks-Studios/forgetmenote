// Command fixture materializes the storage benchmark fixtures for
// baselines and import tests:
//
//	go run ./internal/benchfixture/cmd/fixture -fixture f2 -format git -out /tmp/f2
//
// Fixtures: f1 (the novel note), f1h (F1 with its edit history), f2 (the
// generated notebook) and t1 (JSON edit traces). Formats: git (a repository
// with a working tree), bare, or files (the final files without history).
// F1, F1h and T1 read the novel from FMN_FIXTURE_CACHE; set
// FMN_FIXTURE_FETCH=1 to download it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"iter"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"forgetmenote/internal/benchfixture"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "fixture:", err)
		os.Exit(1)
	}
}

type summary struct {
	Fixture   string  `json:"fixture"`
	Format    string  `json:"format"`
	Out       string  `json:"out"`
	Seed      uint64  `json:"seed"`
	Revisions int     `json:"revisions,omitempty"`
	Head      string  `json:"head,omitempty"`
	Files     int     `json:"files,omitempty"`
	Seconds   float64 `json:"seconds"`
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("fixture", flag.ContinueOnError)
	fixture := flags.String("fixture", "", "f1, f1h, f2 or t1")
	format := flags.String("format", "git", "git, bare or files (f1h and f2)")
	out := flags.String("out", "", "output directory; must be empty or absent")
	seed := flags.Uint64("seed", 1, "generator seed")
	edits := flags.Int("edits", benchfixture.DefaultNovelEdits, "F1h edit revisions")
	revisions := flags.Int("revisions", benchfixture.DefaultNotebookRevisions, "F2 commits, init included")
	notes := flags.Int("notes", benchfixture.DefaultNotebookNotes, "F2 notes")
	assets := flags.Int("assets", benchfixture.DefaultNotebookAssets, "F2 images")
	minAsset := flags.Int("min-asset-bytes", 1<<20, "smallest F2 image")
	maxAsset := flags.Int("max-asset-bytes", 5<<20, "largest F2 image")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *out == "" || flags.NArg() > 0 {
		return errors.New("usage: fixture -fixture f1|f1h|f2|t1 [-format git|bare|files] -out DIR")
	}
	started := time.Now()
	s := summary{Fixture: *fixture, Format: *format, Out: *out, Seed: *seed}
	var revs iter.Seq[benchfixture.Revision]
	switch *fixture {
	case "f1", "f1h", "t1":
		novel, err := benchfixture.Novel(ctx)
		if err != nil {
			return err
		}
		switch *fixture {
		case "f1":
			s.Format, s.Files = "files", 1
			if err = writeFile(filepath.Join(*out, benchfixture.NovelFile), novel); err != nil {
				return err
			}
		case "t1":
			s.Format = "json"
			traces, err := benchfixture.Traces(novel, *seed)
			if err != nil {
				return err
			}
			for _, t := range traces {
				raw, err := json.MarshalIndent(t, "", " ")
				if err != nil {
					return err
				}
				if err = writeFile(filepath.Join(*out, "t1-"+t.Name+".json"), append(raw, '\n')); err != nil {
					return err
				}
			}
			s.Files = len(traces)
		default:
			if revs, err = benchfixture.NovelHistory(novel, benchfixture.NovelHistoryOptions{Seed: *seed, Edits: *edits}); err != nil {
				return err
			}
		}
	case "f2":
		var err error
		revs, err = benchfixture.Notebook(benchfixture.NotebookOptions{Seed: *seed, Notes: *notes, Assets: *assets, Revisions: *revisions, MinAssetBytes: *minAsset, MaxAssetBytes: *maxAsset})
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown fixture %q", *fixture)
	}
	if revs != nil {
		switch *format {
		case "git", "bare":
			result, err := benchfixture.WriteGit(ctx, *out, revs, *format == "bare")
			if err != nil {
				return err
			}
			s.Revisions, s.Head = result.Revisions, result.Head
		case "files":
			n, err := benchfixture.WriteFiles(*out, revs)
			if err != nil {
				return err
			}
			s.Revisions = n
		default:
			return fmt.Errorf("unknown format %q", *format)
		}
	}
	s.Seconds = time.Since(started).Seconds()
	return json.NewEncoder(stdout).Encode(s)
}

func writeFile(path string, content []byte) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s exists", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, content, 0o644)
}
