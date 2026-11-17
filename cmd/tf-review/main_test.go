package main

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/terraform-agent/internal/config"
	"github.com/terraform-agent/internal/input"
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

const (
	testSHA     = "0123456789abcdef0123456789abcdef01234567"
	testProject = 4242
)

// executeWith runs execute against a fake GitLab serving mrJSON and a
// pipeline list containing pipeline 900 at testSHA. The MCP binary does not
// exist, so any error mentioning tfmcp proves tfmcp.Start was reached.
func executeWith(t *testing.T, mrJSON string) error {
	t.Helper()
	mux := http.NewServeMux()
	base := fmt.Sprintf("/api/v4/projects/%d/merge_requests/7", testProject)
	mux.HandleFunc(base, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, mrJSON) })
	mux.HandleFunc(base+"/pipelines", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `[{"id":900,"sha":%q}]`, testSHA)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("tok-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		AllowedGitlabProjectID: testProject, GitlabBaseURL: srv.URL, TfeAddress: config.TfeAddress,
		McpBinaryPath: filepath.Join(t.TempDir(), "no-such-mcp"),
		TokenFiles:    config.TokenFiles{TFE: tok, GitLab: tok},
	}
	j := job{in: input.Input{MRIID: 7, SHA: testSHA, PipelineID: 900}, cfg: cfg}
	return execute(context.Background(), j, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func mrJSON(sha string, source int) string {
	return fmt.Sprintf(`{"iid":7,"sha":%q,"source_project_id":%d,"target_project_id":%d}`, sha, source, testProject)
}

func TestExecute_PreflightRunsBeforeMCPStart(t *testing.T) {
	if err := executeWith(t, mrJSON(strings.Repeat("f", 40), testProject)); err != nil {
		t.Errorf("stale MR: err = %v, want nil without starting MCP", err)
	}
	err := executeWith(t, mrJSON(testSHA, 999))
	if err == nil || strings.Contains(err.Error(), "tfmcp") {
		t.Errorf("fork MR: err = %v, want preflight error before MCP start", err)
	}
	// Control: a valid MR reaches tfmcp.Start, which fails on the missing binary.
	if err := executeWith(t, mrJSON(testSHA, testProject)); err == nil || !strings.Contains(err.Error(), "tfmcp") {
		t.Errorf("valid MR: err = %v, want tfmcp start error", err)
	}
}

// envFuncs are the env-reading functions per import path.
var envFuncs = map[string][]string{
	"os":      {"Getenv", "LookupEnv", "Environ", "ExpandEnv"},
	"syscall": {"Getenv", "Environ"},
}

// envRead is one env-reading use, or a dot import of os/syscall (which would
// hide calls from the scan). asArg is true when the function is passed as a
// value (call argument) rather than called.
type envRead struct {
	name  string
	file  string
	line  int
	asArg bool
}

func (r envRead) String() string {
	return fmt.Sprintf("%s@%s:%d(asArg=%t)", r.name, r.file, r.line, r.asArg)
}

func envReads(fset *token.FileSet, f *ast.File) []envRead {
	locals, out := envImports(fset, f)
	args := map[ast.Expr]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			for _, a := range call.Args {
				args[a] = true
			}
		}
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if name, ok := envFunc(locals, sel); ok {
			p := fset.Position(sel.Pos())
			out = append(out, envRead{name: name, file: p.Filename, line: p.Line, asArg: args[sel]})
		}
		return true
	})
	return out
}

// envImports maps local import names to os/syscall and reports dot imports.
func envImports(fset *token.FileSet, f *ast.File) (map[string]string, []envRead) {
	locals := map[string]string{}
	var dots []envRead
	for _, imp := range f.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		if _, ok := envFuncs[path]; !ok {
			continue
		}
		local := path
		if imp.Name != nil {
			local = imp.Name.Name
		}
		if local == "." {
			p := fset.Position(imp.Pos())
			dots = append(dots, envRead{name: "dot-import " + path, file: p.Filename, line: p.Line})
			continue
		}
		locals[local] = path
	}
	return locals, dots
}

func envFunc(locals map[string]string, sel *ast.SelectorExpr) (string, bool) {
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", false
	}
	path, ok := locals[id.Name]
	if !ok || !slices.Contains(envFuncs[path], sel.Sel.Name) {
		return "", false
	}
	return path + "." + sel.Sel.Name, true
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
			reads = append(reads, envReads(fset, f)...)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	mainFile := filepath.Join("..", "tf-review", "main.go")
	want := envRead{name: "os.Getenv", file: mainFile, asArg: true}
	if len(reads) != 1 || reads[0].name != want.name || reads[0].file != want.file || !reads[0].asArg {
		t.Fatalf("env reads = %v, want exactly os.Getenv passed as an argument in main.go", reads)
	}
}

func scanSource(t *testing.T, src string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range envReads(fset, f) {
		got = append(got, fmt.Sprintf("%s/%t", r.name, r.asArg))
	}
	return got
}

func TestEnvReads_DetectsCallsAndAliases(t *testing.T) {
	got := scanSource(t, `package x
import (
	sys "os"
	"syscall"
)
func f() {
	_ = sys.Getenv("A"); _, _ = sys.LookupEnv("B"); g(sys.Getenv); g(sys.Getenv("C"))
	_ = sys.Environ(); _ = sys.ExpandEnv("$D"); _ = sys.Getpid()
	_ = syscall.Environ(); _, _ = syscall.Getenv("E"); g(syscall.Getenv)
}
func g(any) {}`)
	want := []string{
		"os.Getenv/false", "os.LookupEnv/false", "os.Getenv/true", "os.Getenv/false",
		"os.Environ/false", "os.ExpandEnv/false",
		"syscall.Environ/false", "syscall.Getenv/false", "syscall.Getenv/true",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("reads = %v\nwant    %v", got, want)
	}
}

func TestEnvReads_DotImportsAreReported(t *testing.T) {
	got := scanSource(t, `package x
import (
	. "os"
	. "syscall"
)`)
	want := []string{"dot-import os/false", "dot-import syscall/false"}
	if !slices.Equal(got, want) {
		t.Fatalf("reads = %v, want %v", got, want)
	}
}
