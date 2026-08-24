package ratelimit

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/imlargo/ratelimit"

// TestRootModuleHasNoDependencies is the promise checked mechanically rather
// than made in a README.
//
// Using this package with the standard library must not drag in Redis,
// Prometheus, a web framework or a tracer. Everything that adds a require lives
// in a satellite module with its own go.mod.
func TestRootModuleHasNoDependencies(t *testing.T) {
	b, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(b), "\n") {
		l := strings.TrimSpace(line)
		switch {
		case l == "", strings.HasPrefix(l, "//"):
			continue
		case strings.HasPrefix(l, "module "), strings.HasPrefix(l, "go "), strings.HasPrefix(l, "toolchain "):
			continue
		default:
			t.Errorf("go.mod line %d is %q; the root module must contain nothing but module and go directives", i+1, l)
		}
	}
	if _, err := os.Stat("go.sum"); err == nil {
		t.Error("go.sum exists; the root module must have no dependencies to lock")
	}
}

// TestRootModuleImportsOnlyTheStandardLibrary catches a dependency that arrives
// through an import before it ever reaches go.mod.
//
// The heuristic is exact for its purpose: a module path always has a dot in its
// first element, because it starts with a domain, and no standard library path
// ever does.
func TestRootModuleImportsOnlyTheStandardLibrary(t *testing.T) {
	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "backends", "metrics", "adapters", "docs", "context":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if p == modulePath || strings.HasPrefix(p, modulePath+"/") {
				continue // our own packages
			}
			first := p
			if i := strings.IndexByte(p, '/'); i >= 0 {
				first = p[:i]
			}
			if strings.Contains(first, ".") {
				t.Errorf("%s imports %q, which is outside the standard library. "+
					"Anything that adds a require belongs in a satellite module with its own go.mod",
					path, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestNoReflectionOnTheDecisionPath. Reflection in the hot path is a promise
// this package makes, so it is checked rather than remembered.
func TestNoReflectionOnTheDecisionPath(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			switch p {
			case "reflect", "unsafe":
				// unsafe is allowed only where a test needs Sizeof.
				if p == "unsafe" && strings.HasSuffix(path, "_test.go") {
					continue
				}
				t.Errorf("%s imports %q", path, p)
			}
		}
	}
}
