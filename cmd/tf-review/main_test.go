package main

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
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

func TestRun_GODEBUGFailsFast(t *testing.T) {
	var asked []string
	getenv := func(k string) string {
		asked = append(asked, k)
		if k == envGODEBUG {
			return "http2debug=1"
		}
		return taskEnv(k)
	}
	err := run(context.Background(), strings.Fields(validArgs), getenv)
	if err == nil || !strings.Contains(err.Error(), "GODEBUG") {
		t.Fatalf("err = %v, want GODEBUG error", err)
	}
	if !slices.Equal(asked, []string{envGODEBUG}) {
		t.Fatalf("env reads = %v, want GODEBUG checked first and alone", asked)
	}
	if strings.Contains(err.Error(), "http2debug") {
		t.Fatalf("error echoes the env value: %v", err)
	}
}

// TestRun_ReadsOnlyFixedEnvKeys pins the env keys the program reads itself.
// Standard and library code read others implicitly (see the operations doc).
func TestRun_ReadsOnlyFixedEnvKeys(t *testing.T) {
	keys := map[string]bool{}
	_ = run(context.Background(), strings.Fields(validArgs), func(k string) string {
		keys[k] = true
		return taskEnv(k)
	})
	want := map[string]bool{envGODEBUG: true, "CLOUD_RUN_TASK_COUNT": true, "CLOUD_RUN_TASK_INDEX": true}
	if len(keys) != len(want) {
		t.Fatalf("env keys read = %v, want %v", keys, want)
	}
	for k := range want {
		if !keys[k] {
			t.Fatalf("env keys read = %v, want %v", keys, want)
		}
	}
}

func TestRun_DiscardsStandardLogger(t *testing.T) {
	orig := log.Writer()
	defer log.SetOutput(orig)
	log.SetOutput(os.Stderr)
	_ = run(context.Background(), []string{"--mr-iid=x"}, taskEnv)
	if log.Writer() != io.Discard {
		t.Fatal("run must route the standard logger to io.Discard")
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

// noteServer is a stateful fake of the GitLab notes endpoints.
type noteServer struct {
	mu     sync.Mutex
	bodies []string
}

func (n *noteServer) handle(w http.ResponseWriter, r *http.Request) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if r.Method == http.MethodPost {
		var req struct {
			Body string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		n.bodies = append(n.bodies, req.Body)
		fmt.Fprint(w, noteJSON(len(n.bodies), req.Body))
		return
	}
	parts := make([]string, len(n.bodies))
	for i, b := range n.bodies {
		parts[i] = noteJSON(i+1, b)
	}
	fmt.Fprint(w, "["+strings.Join(parts, ",")+"]")
}

func noteJSON(id int, body string) string {
	b, _ := json.Marshal(body)
	return fmt.Sprintf(`{"id":%d,"body":%s,"author":{"id":1}}`, id, b)
}

// executeWith runs execute against a fake GitLab serving mrJSON and a
// pipeline list containing pipeline 900 at testSHA. The MCP binary does not
// exist, so any error mentioning tfmcp proves tfmcp.Start was reached. It
// returns the bodies of the notes posted.
func executeWith(t *testing.T, mrJSON string) ([]string, error) {
	t.Helper()
	mux := http.NewServeMux()
	base := fmt.Sprintf("/api/v4/projects/%d/merge_requests/7", testProject)
	mux.HandleFunc(base, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, mrJSON) })
	mux.HandleFunc(base+"/pipelines", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `[{"id":900,"sha":%q}]`, testSHA)
	})
	mux.HandleFunc("/api/v4/user", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"id":1}`) })
	notes := &noteServer{}
	mux.HandleFunc(base+"/notes", notes.handle)
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
		Workspaces:    []string{"ws-a"},
	}
	j := job{in: input.Input{MRIID: 7, SHA: testSHA, PipelineID: 900}, cfg: cfg}
	err := execute(context.Background(), j, slog.New(slog.NewTextHandler(io.Discard, nil)))
	notes.mu.Lock()
	defer notes.mu.Unlock()
	return notes.bodies, err
}

func mrJSON(sha string, source int) string {
	return fmt.Sprintf(`{"iid":7,"sha":%q,"source_project_id":%d,"target_project_id":%d}`, sha, source, testProject)
}

func TestExecute_PreflightRunsBeforeMCPStart(t *testing.T) {
	if notes, err := executeWith(t, mrJSON(strings.Repeat("f", 40), testProject)); err != nil || len(notes) != 0 {
		t.Errorf("stale MR: err = %v, notes = %d; want nil without starting MCP or posting", err, len(notes))
	}
	notes, err := executeWith(t, mrJSON(testSHA, 999))
	if err == nil || strings.Contains(err.Error(), "tfmcp") || len(notes) != 0 {
		t.Errorf("fork MR: err = %v, notes = %d; want preflight error before MCP start, no notes", err, len(notes))
	}
	// Control: a valid MR reaches tfmcp.Start, which fails on the missing binary.
	if _, err := executeWith(t, mrJSON(testSHA, testProject)); err == nil || !strings.Contains(err.Error(), "tfmcp") {
		t.Errorf("valid MR: err = %v, want tfmcp start error", err)
	}
}

// A start failure after Preflight replaces the notes with analysis-failed
// notices (code and every workspace), without the error text.
func TestExecute_StartFailurePostsAnalysisFailedNotes(t *testing.T) {
	notes, err := executeWith(t, mrJSON(testSHA, testProject))
	if err == nil {
		t.Fatal("want tfmcp start error")
	}
	if len(notes) != 2 {
		t.Fatalf("notes = %d, want code + plan", len(notes))
	}
	for _, b := range notes {
		if !strings.Contains(b, "분석 실패") || strings.Contains(b, "no-such-mcp") || strings.Contains(b, "tfmcp") {
			t.Errorf("unexpected note body:\n%s", b)
		}
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
