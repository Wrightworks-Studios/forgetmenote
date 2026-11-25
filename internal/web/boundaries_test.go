package web

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// productionFiles parses every non-test Go file in the module, skipping
// hidden directories, node_modules and testdata.
func productionFiles(t *testing.T, mode parser.Mode) map[string]*ast.File {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatal("module root not found:", err)
	}
	files := map[string]*ast.File{}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); p != root && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), p, nil, mode)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		files[filepath.ToSlash(rel)] = file
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 100 {
		t.Fatalf("parsed only %d files", len(files))
	}
	return files
}

// TestOnlyWebServesMCP keeps tool registration in web, where addAction applies
// the one authorization path every transport shares. Another internal package
// exports typed operations instead; connectors is an MCP client of data
// connections.
func TestOnlyWebServesMCP(t *testing.T) {
	for rel, file := range productionFiles(t, parser.ImportsOnly) {
		if !strings.HasPrefix(rel, "internal/") || strings.HasPrefix(rel, "internal/web/") || strings.HasPrefix(rel, "internal/connectors/") {
			continue
		}
		for _, spec := range file.Imports {
			if path, _ := strconv.Unquote(spec.Path.Value); strings.HasPrefix(path, "github.com/modelcontextprotocol/go-sdk") {
				t.Errorf("%s imports %s; only web registers MCP tools", rel, path)
			}
		}
	}
}

// TestContextAuthorityHasKnownSetters lists every production file that stores,
// replaces or clears a chat run's scope or an automation's identity. Those
// values decide what a run may read and change: an automation's identity is
// the authority for its own writes and admits its code-only connections, a
// person's approval lifts the personal-context refusal, and a wider or missing
// chat scope lifts the run's corpus limits. Reading them is free. A new setter
// belongs here only where the check before it is the run's own.
func TestContextAuthorityHasKnownSetters(t *testing.T) {
	setters := map[string][]string{
		"forgetmenote/internal/automation.WithIdentity": {"internal/web/automation.go", "internal/web/source_jobs.go"},
		"forgetmenote/internal/chat.WithRunScope":       {"internal/web/chat_context.go", "internal/web/chat_controls.go", "internal/web/chat_review.go", "internal/web/chat_round.go", "internal/web/chat_runner.go", "internal/web/jev_citations.go"},
		"forgetmenote/internal/chat.WithScope":          {"internal/web/automation_assistant.go", "internal/web/chat_action_scope.go", "internal/web/chat_repository.go", "internal/web/chat_scope.go"},
		"forgetmenote/internal/chat.WithApproval":       {"internal/web/chat_controls.go", "internal/web/chat_runner.go"},
		"forgetmenote/internal/chat.WithoutRunScope":    {"internal/web/automation.go", "internal/web/automation_script_capabilities.go"},
	}
	owners := map[string]bool{}
	for setter := range setters {
		owners[setter[:strings.LastIndex(setter, ".")]] = true
	}
	found := map[string][]string{}
	use := func(setter, rel string) {
		if _, ok := setters[setter]; ok && !slices.Contains(found[setter], rel) {
			found[setter] = append(found[setter], rel)
		}
	}
	for rel, file := range productionFiles(t, 0) {
		own := "forgetmenote/" + filepath.ToSlash(filepath.Dir(rel))
		names := map[string]string{}
		for _, spec := range file.Imports {
			path, _ := strconv.Unquote(spec.Path.Value)
			name := path[strings.LastIndex(path, "/")+1:]
			if spec.Name != nil {
				name = spec.Name.Name
			}
			if name == "." && owners[path] {
				t.Errorf("%s dot-imports %s; name its context setters explicitly", rel, path)
			}
			names[name] = path
		}
		// Any reference counts, not only a call: a setter passed as a value
		// stores authority just the same.
		var visit func(ast.Node) bool
		visit = func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				// A declaration's own name is not a use.
				if n.Recv != nil {
					ast.Inspect(n.Recv, visit)
				}
				ast.Inspect(n.Type, visit)
				if n.Body != nil {
					ast.Inspect(n.Body, visit)
				}
				return false
			case *ast.SelectorExpr:
				if x, ok := n.X.(*ast.Ident); ok && names[x.Name] != "" {
					use(names[x.Name]+"."+n.Sel.Name, rel)
					return false
				}
			case *ast.Ident:
				use(own+"."+n.Name, rel)
			}
			return true
		}
		ast.Inspect(file, visit)
	}
	for setter, allowed := range setters {
		for _, rel := range found[setter] {
			if !slices.Contains(allowed, rel) {
				t.Errorf("%s uses %s; list it here only if the check before it is the run's own", rel, setter)
			}
		}
		for _, rel := range allowed {
			if !slices.Contains(found[setter], rel) {
				t.Errorf("%s no longer uses %s; remove it from the list", rel, setter)
			}
		}
	}
}
