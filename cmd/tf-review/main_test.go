package main

import (
	"context"
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

const validArgs = "--mr-iid=7 --sha=0123456789abcdef0123456789abcdef01234567 --pipeline-id=900"

func taskEnv(key string) string {
	return map[string]string{"CLOUD_RUN_TASK_COUNT": "1", "CLOUD_RUN_TASK_INDEX": "0"}[key]
}

func TestRun_BadTaskEnvFailsBeforeConfig(t *testing.T) {
	err := run(context.Background(), strings.Fields(validArgs), func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "CLOUD_RUN_TASK_COUNT") {
		t.Fatalf("err = %v, want task env error", err)
	}
}

func TestRun_BadArgsFail(t *testing.T) {
	err := run(context.Background(), []string{"--mr-iid=x"}, taskEnv)
	if err == nil || !strings.Contains(err.Error(), "mr-iid") {
		t.Fatalf("err = %v, want mr-iid error", err)
	}
}

func TestRun_MissingConfigFails(t *testing.T) {
	if _, err := os.Stat(configPath); err == nil {
		t.Skipf("%s exists on this machine", configPath)
	}
	err := run(context.Background(), strings.Fields(validArgs), taskEnv)
	if err == nil || !strings.Contains(err.Error(), configPath) {
		t.Fatalf("err = %v, want config read error", err)
	}
}

func TestReadToken_TrimsLikeTheClients(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte("  tok-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readToken(p)
	if err != nil || got != "tok-123" {
		t.Fatalf("readToken = %q, %v", got, err)
	}
	if _, err := readToken(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing file: want error")
	}
}

// envReads lists every os.Getenv/os.LookupEnv use in a file, recording
// whether it is passed as a value (call argument) rather than called.
type envRead struct {
	file  string
	line  int
	asArg bool
}

func envReads(t *testing.T, fset *token.FileSet, f *ast.File) []envRead {
	t.Helper()
	osName := ""
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == "os" {
			osName = "os"
			if imp.Name != nil {
				osName = imp.Name.Name
			}
		}
	}
	if osName == "" {
		return nil
	}
	isEnv := func(e ast.Expr) bool {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "Getenv" && sel.Sel.Name != "LookupEnv") {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && id.Name == osName
	}
	args := map[ast.Expr]bool{}
	var out []envRead
	ast.Inspect(f, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			for _, a := range call.Args {
				args[a] = true
			}
		}
		if e, ok := n.(ast.Expr); ok && isEnv(e) {
			p := fset.Position(e.Pos())
			out = append(out, envRead{file: p.Filename, line: p.Line, asArg: args[e]})
		}
		return true
	})
	return out
}

// testdata is skipped: the Go tool never builds it into the binary (it holds
// the fake MCP server used by tfmcp tests).
func TestNoEnvReadsExceptInputParseInMain(t *testing.T) {
	fset := token.NewFileSet()
	var reads []envRead
	for _, root := range []string{"..", filepath.Join("..", "..", "internal")} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == "testdata" {
				return filepath.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			reads = append(reads, envReads(t, fset, f)...)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	mainFile := filepath.Join("..", "tf-review", "main.go")
	if len(reads) != 1 || !reads[0].asArg || reads[0].file != mainFile {
		t.Fatalf("env reads = %+v, want exactly os.Getenv passed as an argument in main.go", reads)
	}
}

func TestEnvReads_DetectsCallsAndAliases(t *testing.T) {
	src := `package x
import sys "os"
func f() { _ = sys.Getenv("A"); _, _ = sys.LookupEnv("B"); g(sys.Getenv); g(sys.Getenv("C")) }
func g(any) {}`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var asArg []bool
	for _, r := range envReads(t, fset, f) {
		asArg = append(asArg, r.asArg)
	}
	if want := []bool{false, false, true, false}; !slices.Equal(asArg, want) {
		t.Fatalf("asArg = %v, want %v", asArg, want)
	}
}
