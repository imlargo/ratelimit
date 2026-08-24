package ratelimit

import (
	"go/ast"
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

// TestNoExportedMutablePackageState. This package says it has no global mutable
// state, so the claim is checked rather than remembered.
//
// It matters most for the trusted-proxy list. As a package-level slice, anything
// else in the process could have appended 0.0.0.0/0 to it and silently disabled
// spoofing protection for every caller, with nothing to see in a diff of your
// own code. It is a function returning a fresh slice for that reason.
//
// Sentinel errors are the one exception: errors.New cannot be a constant, so
// every Go package in existence exports them as variables.
func TestNoExportedMutablePackageState(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, name := range vs.Names {
					if !name.IsExported() {
						continue
					}
					if strings.HasPrefix(name.Name, "Err") {
						continue // sentinel error
					}
					t.Errorf("%s declares the exported variable %s. Anything in the process can "+
						"reassign it, which is global mutable state; export a function returning a "+
						"fresh value instead", path, name.Name)
				}
			}
		}
	}
}

// TestPrivateRangesIsFreshEachCall, so a caller mutating what it gets back
// cannot affect anyone else.
func TestPrivateRangesIsFreshEachCall(t *testing.T) {
	a, b := PrivateRanges(), PrivateRanges()
	if len(a) == 0 || len(a) != len(b) {
		t.Fatalf("PrivateRanges returned %d and %d entries", len(a), len(b))
	}
	a[0] = "0.0.0.0/0"
	if b[0] == "0.0.0.0/0" {
		t.Error("two calls to PrivateRanges share backing storage; one caller could widen another's trusted ranges")
	}
	if PrivateRanges()[0] == "0.0.0.0/0" {
		t.Error("mutating the result of PrivateRanges changed what later calls return")
	}
}
