package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
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
// replaces or clears a chat run's scope or an automation's identity, or marks
// a script's host call. Those values decide what a run may read and change: an
// automation's identity is the authority for its own writes and admits its
// code-only connections, the script-host mark admits them inside a chat scope,
// a person's approval lifts the personal-context refusal, and a wider or
// missing chat scope lifts the run's corpus limits. Reading them is free. A new
// setter belongs here only where the check before it is the run's own.
func TestContextAuthorityHasKnownSetters(t *testing.T) {
	setters := map[string][]string{
		"forgetmenote/internal/automation.WithIdentity":   {"internal/web/automation.go", "internal/web/source_jobs.go"},
		"forgetmenote/internal/automation.WithScriptHost": {"internal/automationrun/script.go"},
		"forgetmenote/internal/chat.WithRunScope":         {"internal/chatrun/citations.go", "internal/chatrun/context.go", "internal/chatrun/controls.go", "internal/chatrun/review.go", "internal/chatrun/round.go", "internal/chatrun/runner.go"},
		"forgetmenote/internal/chat.WithScope":            {"internal/automationrun/scope.go", "internal/chatrun/repository.go", "internal/chatrun/scope.go", "internal/web/chat_action_scope.go"},
		"forgetmenote/internal/chat.WithApproval":         {"internal/chatrun/controls.go", "internal/chatrun/runner.go"},
		"forgetmenote/internal/chat.WithoutRunScope":      {"internal/automationrun/publish.go", "internal/automationrun/script_capabilities.go"},
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

// TestCredentialScopeHasKnownSetters lists every production function that
// stores a caller's scope under auth.ScopeKey, which web names scopeKey. The
// scope bounds what an access token, a credential or an automation may read
// and write, and a context without one is unrestricted. Each store admits a
// caller or sets a scope on purpose: an OAuth access token or a credential
// when it is admitted, an automation when its context starts, a source job
// restoring the scope it recorded, and a script's guarded connector
// submission, which widens its run to data:write for that one write. A new
// setter belongs here only where the check before it decides that scope. The
// check follows types, so an alias of the key counts, and the key may only be
// read with Value or stored with context.WithValue, so it cannot reach another
// store through a variable.
func TestCredentialScopeHasKnownSetters(t *testing.T) {
	setters := []string{
		"internal/automationrun/script_capabilities.go (*Service).scriptSubmit",
		"internal/web/automation.go (*Server).automationContext",
		"internal/web/credentials.go (*Server).credentialToken",
		"internal/web/oauth.go (*Server).validAccessToken",
		"internal/web/source_jobs.go (*Server).sourceContext",
	}
	code := typedAuthDependents(t)
	isKey := func(obj types.Object) bool {
		name, ok := obj.(*types.TypeName)
		if !ok {
			return false
		}
		named, ok := types.Unalias(name.Type()).(*types.Named)
		return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "forgetmenote/internal/auth" && named.Obj().Name() == "ScopeKey"
	}
	// use classifies the key named at the top of stack: an alias declaration,
	// a read with Value, a store with context.WithValue, or anything else.
	use := func(stack []ast.Node) string {
		i := len(stack) - 1
		expr := stack[i]
		if sel, ok := stack[i-1].(*ast.SelectorExpr); ok && sel.Sel == expr {
			expr, i = sel, i-1
		}
		switch parent := stack[i-1].(type) {
		case *ast.TypeSpec:
			if parent.Assign.IsValid() && parent.Type == expr {
				return "alias"
			}
		case *ast.CompositeLit:
			call, ok := stack[i-2].(*ast.CallExpr)
			if parent.Type != expr || len(parent.Elts) > 0 || !ok {
				return ""
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return ""
			}
			fn, ok := code.info.Uses[sel.Sel].(*types.Func)
			if !ok || fn.Pkg() == nil || fn.Pkg().Path() != "context" {
				return ""
			}
			if fn.Name() == "WithValue" && len(call.Args) == 3 && call.Args[1] == parent {
				return "store"
			}
			if fn.Name() == "Value" && len(call.Args) == 1 {
				return "read"
			}
		}
		return ""
	}
	found := map[string]int{}
	for _, files := range code.files {
		for _, file := range files {
			var stack []ast.Node
			ast.Inspect(file, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return true
				}
				stack = append(stack, n)
				id, ok := n.(*ast.Ident)
				if !ok || !isKey(code.info.Uses[id]) {
					return true
				}
				where := code.rel(id) + " " + enclosingFunc(stack)
				switch use(stack) {
				case "store":
					found[where]++
				case "":
					t.Errorf("%s uses auth.ScopeKey other than to read it with Value or store it with context.WithValue", where)
				}
				return true
			})
		}
	}
	want := map[string]int{}
	for _, where := range setters {
		want[where]++
	}
	for _, where := range slices.Sorted(maps.Keys(found)) {
		if found[where] != want[where] {
			t.Errorf("%s stores the credential scope %d times, %d listed; list a setter here only if the check before it decides that scope", where, found[where], want[where])
		}
	}
	for _, where := range setters {
		if found[where] == 0 {
			t.Errorf("%s no longer stores the credential scope; remove it from the list", where)
		}
	}
}

// enclosingFunc names the function declaration in stack, such as
// (*Server).scriptSubmit, or reports package scope.
func enclosingFunc(stack []ast.Node) string {
	for i := len(stack) - 1; i >= 0; i-- {
		fn, ok := stack[i].(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Recv == nil || len(fn.Recv.List) == 0 {
			return fn.Name.Name
		}
		recv := fn.Recv.List[0].Type
		star := ""
		if s, ok := recv.(*ast.StarExpr); ok {
			recv, star = s.X, "*"
		}
		if index, ok := recv.(*ast.IndexExpr); ok {
			recv = index.X
		} else if index, ok := recv.(*ast.IndexListExpr); ok {
			recv = index.X
		}
		if id, ok := recv.(*ast.Ident); ok {
			return "(" + star + id.Name + ")." + fn.Name.Name
		}
		return fn.Name.Name
	}
	return "package scope"
}

// TestGrantsBeginOnlyInConnectionRoutes keeps personal connection grants in
// web's connect, callback and disconnect routes in personal_connections.go,
// which bind them to the browser with a cookie and recheck the signed-in
// account: no other production code may begin, finish or remove one. The
// check follows types, so a method of the same name elsewhere is not a use and
// one reached through a hook or a local variable is.
func TestGrantsBeginOnlyInConnectionRoutes(t *testing.T) {
	code := typedConnectorDependents(t)
	grants := []string{"Begin", "Disconnect", "Finish"}
	var found []string
	for id, obj := range code.info.Uses {
		fn, ok := obj.(*types.Func)
		if !ok || !slices.Contains(grants, fn.Name()) || !receiverIs(fn, "forgetmenote/internal/connectors", "PersonalConnections") {
			continue
		}
		if rel := code.rel(id); rel != "internal/web/personal_connections.go" {
			t.Errorf("%s uses PersonalConnections.%s; only the connection routes in personal_connections.go may", rel, fn.Name())
			continue
		}
		found = append(found, fn.Name())
	}
	slices.Sort(found)
	if found = slices.Compact(found); !slices.Equal(found, grants) {
		t.Fatalf("personal_connections.go uses %v of %v", found, grants)
	}
}

// TestBindingCookiesStayInWeb keeps the cookies that bind a pending login or
// connection, with its OAuth state, to the browser that started it in web:
// binding_cookie.go alone declares the helpers, none of them is exported, and
// no production code outside web sets or reads a cookie.
func TestBindingCookiesStayInWeb(t *testing.T) {
	helpers := []string{"binding", "bindingCookie", "clearBinding", "connectionBinding", "setBinding"}
	const owner = "internal/web/binding_cookie.go"
	var found []string
	for rel, file := range productionFiles(t, 0) {
		var names []*ast.Ident
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				names = append(names, d.Name)
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch spec := spec.(type) {
					case *ast.TypeSpec:
						names = append(names, spec.Name)
					case *ast.ValueSpec:
						names = append(names, spec.Names...)
					}
				}
			}
		}
		for _, name := range names {
			switch {
			case rel == owner && name.IsExported():
				t.Errorf("%s exports %s; binding cookie helpers stay unexported", rel, name.Name)
			case rel == owner && slices.Contains(helpers, name.Name):
				found = append(found, name.Name)
			case slices.Contains(helpers, name.Name):
				t.Errorf("%s declares %s; binding cookie helpers belong in %s", rel, name.Name, owner)
			}
		}
		if rel == owner && file.Name.Name != "web" {
			t.Errorf("%s is package %s, not web", rel, file.Name.Name)
		}
		if strings.HasPrefix(rel, "internal/web/") {
			continue
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && slices.Contains([]string{"AddCookie", "Cookie", "Cookies", "SetCookie"}, sel.Sel.Name) {
				t.Errorf("%s uses %s; only web sets or reads cookies", rel, sel.Sel.Name)
			}
			return true
		})
	}
	slices.Sort(found)
	if !slices.Equal(found, helpers) {
		t.Fatalf("%s declares %v of %v", owner, found, helpers)
	}
}

// TestAssistantTestExportsStayInTests keeps the names the personal assistant
// exports only for web's tests out of production code: any reference from a
// non-test file in the module fails. The check follows types, so another
// package's Notify or Collect is not a use.
func TestAssistantTestExportsStayInTests(t *testing.T) {
	code := typedConnectorDependents(t)
	const path = "forgetmenote/internal/personal"
	testOnly := []string{"Collect", "Fingerprint", "MaintainMemory", "Notify", "PublishDrafts"}
	pkg := code.packages[path]
	if pkg == nil {
		t.Fatalf("%s was not type-checked", path)
	}
	service, _ := pkg.Scope().Lookup("Service").(*types.TypeName)
	for _, name := range testOnly {
		obj := pkg.Scope().Lookup(name)
		if obj == nil && service != nil {
			obj, _, _ = types.LookupFieldOrMethod(types.NewPointer(service.Type()), false, pkg, name)
		}
		if _, ok := obj.(*types.Func); !ok {
			t.Errorf("personal no longer declares %s; update the list", name)
		}
	}
	for id, obj := range code.info.Uses {
		fn, ok := obj.(*types.Func)
		if !ok || fn.Pkg() == nil || fn.Pkg().Path() != path || !slices.Contains(testOnly, fn.Name()) {
			continue
		}
		if fn.Signature().Recv() == nil || receiverIs(fn, path, "Service") {
			t.Errorf("%s uses personal's %s, which only tests may use", code.rel(id), fn.Name())
		}
	}
}

// receiverIs reports whether fn is a method of path.name or *path.name.
func receiverIs(fn *types.Func, path, name string) bool {
	recv := fn.Signature().Recv()
	if recv == nil {
		return false
	}
	typ := recv.Type()
	if ptr, ok := typ.(*types.Pointer); ok {
		typ = ptr.Elem()
	}
	named, ok := types.Unalias(typ).(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == path && named.Obj().Name() == name
}

// typedCode is the type-checked production code of every module package that
// is or depends on connectors, which holds personal connections and their
// grants and which the chat run engine uses, with each package's files.
type typedCode struct {
	root     string
	fset     *token.FileSet
	info     *types.Info
	packages map[string]*types.Package
	files    map[string][]*ast.File
}

// rel returns the module-relative file that holds id.
func (c *typedCode) rel(id *ast.Ident) string {
	rel, err := filepath.Rel(c.root, c.fset.Position(id.Pos()).Filename)
	if err != nil {
		return c.fset.Position(id.Pos()).Filename
	}
	return filepath.ToSlash(rel)
}

var typedOnce struct {
	sync.Once
	code *typedCode
	err  error
}

func typedConnectorDependents(t *testing.T) *typedCode {
	t.Helper()
	typedOnce.Do(func() { typedOnce.code, typedOnce.err = typeCheckDependents("forgetmenote/internal/connectors") })
	if typedOnce.err != nil {
		t.Fatal(typedOnce.err)
	}
	return typedOnce.code
}

var typedAuthOnce struct {
	sync.Once
	code *typedCode
	err  error
}

// typedAuthDependents is the type-checked production code of auth, which
// declares the context keys of an admitted caller, and of every module package
// that depends on it, which is every package that can name those keys.
func typedAuthDependents(t *testing.T) *typedCode {
	t.Helper()
	typedAuthOnce.Do(func() { typedAuthOnce.code, typedAuthOnce.err = typeCheckDependents("forgetmenote/internal/auth") })
	if typedAuthOnce.err != nil {
		t.Fatal(typedAuthOnce.err)
	}
	return typedAuthOnce.code
}

// typeCheckDependents type-checks, from source and in dependency order, the
// production files of target and of each module package that depends on it.
// Their other imports come from compiled export data. go list builds that
// with the test binary's own race and tag settings, so the build cache that
// made this binary already holds it.
func typeCheckDependents(target string) (*typedCode, error) {
	root, err := filepath.Abs("../..")
	if err != nil {
		return nil, err
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		return nil, err
	}
	var flags []string
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "-race" && s.Value == "true" || s.Key == "-tags" && s.Value != "" {
				flags = append(flags, s.Key+"="+s.Value)
			}
		}
	}
	type listed struct {
		ImportPath, Dir, Export string
		GoFiles, Imports, Deps  []string
		Module                  *struct{ Path string }
	}
	list := func(args ...string) ([]listed, error) {
		cmd := exec.Command(goTool, append(append([]string{"list"}, flags...), args...)...)
		cmd.Dir = root
		out, err := cmd.Output()
		if exit := (*exec.ExitError)(nil); errors.As(err, &exit) {
			return nil, fmt.Errorf("go list: %w\n%s", err, exit.Stderr)
		} else if err != nil {
			return nil, err
		}
		var pkgs []listed
		for dec := json.NewDecoder(bytes.NewReader(out)); ; {
			var p listed
			if err := dec.Decode(&p); err == io.EOF {
				return pkgs, nil
			} else if err != nil {
				return nil, err
			}
			pkgs = append(pkgs, p)
		}
	}
	// -deps lists each package after all of its dependencies.
	all, err := list("-deps", "-json=ImportPath,Dir,GoFiles,Imports,Deps,Module", "./...")
	if err != nil {
		return nil, err
	}
	var order []listed
	fromSource := map[string]bool{}
	for _, p := range all {
		if p.Module != nil && p.Module.Path == "forgetmenote" && (p.ImportPath == target || slices.Contains(p.Deps, target)) {
			order = append(order, p)
			fromSource[p.ImportPath] = true
		}
	}
	if !fromSource[target] {
		return nil, fmt.Errorf("go list found no %s", target)
	}
	// A package outside the set cannot import one inside it, or it would
	// depend on target too, so export data never names a source package.
	need := map[string]bool{}
	for _, p := range order {
		for _, path := range p.Imports {
			if !fromSource[path] && path != "unsafe" && path != "C" {
				need[path] = true
			}
		}
	}
	exported, err := list(append([]string{"-export", "-json=ImportPath,Export"}, slices.Sorted(maps.Keys(need))...)...)
	if err != nil {
		return nil, err
	}
	exports := map[string]string{}
	for _, p := range exported {
		exports[p.ImportPath] = p.Export
	}
	code := &typedCode{root: root, fset: token.NewFileSet(), packages: map[string]*types.Package{}, files: map[string][]*ast.File{},
		info: &types.Info{Uses: map[*ast.Ident]types.Object{}}}
	gc := importer.ForCompiler(code.fset, "gc", func(path string) (io.ReadCloser, error) {
		if exports[path] == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(exports[path])
	})
	conf := types.Config{Importer: importerFunc(func(path string) (*types.Package, error) {
		if pkg := code.packages[path]; pkg != nil {
			return pkg, nil
		}
		return gc.Import(path)
	})}
	for _, p := range order {
		var files []*ast.File
		for _, name := range p.GoFiles {
			file, err := parser.ParseFile(code.fset, filepath.Join(p.Dir, name), nil, parser.SkipObjectResolution)
			if err != nil {
				return nil, err
			}
			files = append(files, file)
		}
		pkg, err := conf.Check(p.ImportPath, code.fset, files, code.info)
		if err != nil {
			return nil, fmt.Errorf("type-check %s: %w", p.ImportPath, err)
		}
		code.packages[p.ImportPath] = pkg
		code.files[p.ImportPath] = files
	}
	return code, nil
}

type importerFunc func(path string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }

// TestMessagingTestExportsStayInTests keeps the names messaging exports only
// for web's tests out of production code, with the account-link token minter,
// which web keeps unexported: any reference from a non-test file anywhere in
// the module fails. A declaration's own name is not a reference.
func TestMessagingTestExportsStayInTests(t *testing.T) {
	testOnly := []string{"MessageConversation", "SendMessage", "SendAttempts", "MessageLinkToken"}
	for rel, file := range productionFiles(t, 0) {
		declared := map[*ast.Ident]bool{}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				declared[d.Name] = true
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if v, ok := spec.(*ast.ValueSpec); ok {
						for _, name := range v.Names {
							declared[name] = true
						}
					}
				}
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && slices.Contains(testOnly, id.Name) && !declared[id] {
				t.Errorf("%s refers to %s, which only tests may use", rel, id.Name)
			}
			return true
		})
	}
}

// TestSlackAppHasOneSignatureGate keeps every Slack app endpoint behind
// slackApp: its first statement checks the request with slackRequest, and it
// alone calls the events, interactions and slash command handlers.
func TestSlackAppHasOneSignatureGate(t *testing.T) {
	handlers := []string{"SlackEvents", "SlackInteractions", "slackCommands"}
	var called []string
	gate := false
	for rel, file := range productionFiles(t, 0) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			inGate := rel == "internal/web/slack_config.go" && fn.Name.Name == "slackApp"
			if inGate {
				gate = true
				first, ok := fn.Body.List[0].(*ast.AssignStmt)
				if !ok || len(first.Rhs) != 1 || calledName(first.Rhs[0]) != "slackRequest" {
					t.Error("slackApp does not start with slackRequest")
				}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && slices.Contains(handlers, sel.Sel.Name) {
					if !inGate {
						t.Errorf("%s: %s calls %s, which only slackApp may call", rel, fn.Name.Name, sel.Sel.Name)
					}
					called = append(called, sel.Sel.Name)
				}
				return true
			})
		}
	}
	slices.Sort(called)
	if !gate || !slices.Equal(called, []string{"SlackEvents", "SlackInteractions", "slackCommands"}) {
		t.Fatalf("slackApp found %v; handlers called %v", gate, called)
	}
}

// calledName returns the name of the function or method e calls, or "".
func calledName(e ast.Expr) string {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return ""
	}
	switch f := call.Fun.(type) {
	case *ast.SelectorExpr:
		return f.Sel.Name
	case *ast.Ident:
		return f.Name
	}
	return ""
}

// TestChatRunRunsToolsOnlyThroughItsHook keeps the chat run engine off the
// action registry and off every other hub's actions: a model's tool call
// reaches an action only through the DispatchAction hook, which web binds to
// its dispatcher (the catalog check, the chat guard and the action). The
// engine cannot import web, so web's addAction and dispatcher are out of its
// reach; it may
// read the registered tools through ActionTool and ChatTools and the policy
// table, but not actions.Registry, Action or TypedCall. A hub's action is a
// method of that hub that a handler web passes to addAction,
// addManagementTool, addProjectTool or notebookJSON reaches, directly or
// through web's own functions. The check follows types, so a function of the
// same name elsewhere is not a use and a method value is.
func TestChatRunRunsToolsOnlyThroughItsHook(t *testing.T) {
	runsToolsOnlyThroughDispatch(t, "chatrun")
}

// TestAutomationRunRunsToolsOnlyThroughItsHook holds the automation run
// engine to the same rule: an automation answer's tool call reaches an action
// only through the engine's DispatchAction hook, which web binds to the same
// dispatcher.
func TestAutomationRunRunsToolsOnlyThroughItsHook(t *testing.T) {
	runsToolsOnlyThroughDispatch(t, "automationrun")
}

// runsToolsOnlyThroughDispatch fails if the engine in internal/name reaches
// the action registry or a hub's action other than through its DispatchAction
// hook, or no longer calls that hook outside its service.go.
func runsToolsOnlyThroughDispatch(t *testing.T, name string) {
	t.Helper()
	code := typedConnectorDependents(t)
	dir, engine := "internal/"+name+"/", "forgetmenote/internal/"+name
	const web = "forgetmenote/internal/web"
	hubs := []string{"forgetmenote/internal/judgments", "forgetmenote/internal/messaging", "forgetmenote/internal/personal"}
	registrars := map[string]int{"addAction": 3, "addManagementTool": 5, "addProjectTool": 5, "notebookJSON": 3}
	server, _ := code.packages[web].Scope().Lookup("Server").(*types.TypeName)
	bodies := map[string]*ast.BlockStmt{}
	for _, file := range code.files[web] {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			var obj types.Object
			if fn.Recv == nil {
				obj = code.packages[web].Scope().Lookup(fn.Name.Name)
			} else if server != nil {
				obj, _, _ = types.LookupFieldOrMethod(types.NewPointer(server.Type()), false, code.packages[web], fn.Name.Name)
			}
			if f, ok := obj.(*types.Func); ok {
				bodies[f.FullName()] = fn.Body
			}
		}
	}
	actions, seen := map[string]bool{}, map[string]bool{}
	var reach func(ast.Node)
	reach = func(n ast.Node) {
		ast.Inspect(n, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			fn, ok := code.info.Uses[id].(*types.Func)
			if !ok || fn.Pkg() == nil {
				return true
			}
			name := fn.Origin().FullName()
			switch {
			case fn.Pkg().Path() == web && bodies[name] != nil && !seen[name]:
				seen[name] = true
				reach(bodies[name])
			case slices.Contains(hubs, fn.Pkg().Path()) && fn.Signature().Recv() != nil:
				actions[name] = true
			}
			return true
		})
	}
	registered := 0
	for _, file := range code.files[web] {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			callee := call.Fun
			if index, ok := callee.(*ast.IndexListExpr); ok {
				callee = index.X
			} else if index, ok := callee.(*ast.IndexExpr); ok {
				callee = index.X
			}
			id, ok := callee.(*ast.Ident)
			if !ok {
				return true
			}
			fn, ok := code.info.Uses[id].(*types.Func)
			if !ok || fn.Pkg() == nil || fn.Pkg().Path() != web {
				return true
			}
			if arg, ok := registrars[fn.Name()]; ok && arg < len(call.Args) {
				registered++
				reach(call.Args[arg])
			}
			return true
		})
	}
	if registered < 100 || len(actions) == 0 {
		t.Fatalf("found %d action registrations reaching %d hub methods; the registrars changed", registered, len(actions))
	}
	registry := []string{"Registry", "Action", "TypedCall", "Schemas"}
	hooks := 0
	for id, obj := range code.info.Uses {
		if !strings.HasPrefix(code.rel(id), dir) || obj.Pkg() == nil {
			continue
		}
		path := obj.Pkg().Path()
		if fn, ok := obj.(*types.Func); ok && actions[fn.Origin().FullName()] {
			t.Errorf("%s calls %s, which a registered action reaches; run tools through the DispatchAction hook", code.rel(id), fn.FullName())
		}
		if path == "forgetmenote/internal/actions" {
			if _, ok := obj.(*types.TypeName); ok && slices.Contains(registry, obj.Name()) {
				t.Errorf("%s uses actions.%s; run tools through the DispatchAction hook", code.rel(id), obj.Name())
			}
			if fn, ok := obj.(*types.Func); ok && (slices.Contains(registry, fn.Name()) || receiverIs(fn, path, "Registry")) {
				t.Errorf("%s uses actions.%s; run tools through the DispatchAction hook", code.rel(id), fn.Name())
			}
			if v, ok := obj.(*types.Var); ok && v.IsField() && v.Name() == "Call" {
				t.Errorf("%s calls an action directly; run tools through the DispatchAction hook", code.rel(id))
			}
		}
		if v, ok := obj.(*types.Var); ok && v.IsField() && v.Name() == "DispatchAction" && path == engine && code.rel(id) != dir+"service.go" {
			hooks++
		}
	}
	if hooks == 0 {
		t.Fatal(name + " no longer runs tools through its DispatchAction hook")
	}
}

// TestChatRunTestExportsStayInTests keeps the names in chatrun's
// testexports.go, which web's server-level tests use to reach engine
// internals, out of production code. Every name that file declares is
// test-only, so a new one is covered when it is added there.
func TestChatRunTestExportsStayInTests(t *testing.T) {
	testExportsStayInTests(t, "chatrun", 20)
}

// TestAutomationRunTestExportsStayInTests does the same for the automation
// run engine's testexports.go.
func TestAutomationRunTestExportsStayInTests(t *testing.T) {
	testExportsStayInTests(t, "automationrun", 20)
}

// testExportsStayInTests fails on a production reference to a name that
// internal/name's testexports.go declares, which must declare at least
// minimum names.
func testExportsStayInTests(t *testing.T, name string, minimum int) {
	t.Helper()
	code := typedConnectorDependents(t)
	path := "forgetmenote/internal/" + name
	pkg := code.packages[path]
	if pkg == nil {
		t.Fatalf("%s was not type-checked", path)
	}
	testOnly := map[string]bool{}
	for _, file := range code.files[path] {
		if filepath.Base(code.fset.Position(file.Pos()).Filename) != "testexports.go" {
			continue
		}
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				testOnly[decl.Name.Name] = true
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					switch spec := spec.(type) {
					case *ast.TypeSpec:
						testOnly[spec.Name.Name] = true
					case *ast.ValueSpec:
						for _, name := range spec.Names {
							testOnly[name.Name] = true
						}
					}
				}
			}
		}
	}
	if len(testOnly) < minimum {
		t.Fatalf("%s's testexports.go declares %d names; was it moved?", name, len(testOnly))
	}
	for id, obj := range code.info.Uses {
		if obj.Pkg() == nil || obj.Pkg().Path() != path || !testOnly[obj.Name()] || code.rel(id) == "internal/"+name+"/testexports.go" {
			continue
		}
		if fn, ok := obj.(*types.Func); ok && fn.Signature().Recv() != nil && !receiverIs(fn, path, "Service") {
			continue
		}
		if _, ok := obj.(*types.Func); !ok && obj.Parent() != pkg.Scope() {
			continue
		}
		t.Errorf("%s uses %s's %s, which only tests may use", code.rel(id), name, obj.Name())
	}
}
