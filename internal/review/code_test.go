package review

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/terraform-agent/internal/config"
	"github.com/terraform-agent/internal/gitlab"
	"github.com/terraform-agent/internal/hclscan"
	"github.com/terraform-agent/internal/input"
	"github.com/terraform-agent/internal/llm"
	"github.com/terraform-agent/internal/model"
	"github.com/terraform-agent/internal/staticcheck"
	"github.com/terraform-agent/internal/tfdocs"
	"github.com/terraform-agent/internal/tfmcp"
)

const (
	headSHA     = "0123456789abcdef0123456789abcdef01234567"
	botID       = int64(42)
	diffSecret  = "DIFF_SENTINEL_9f3a"
	docSecret   = "DOC_SENTINEL_7c1e"
	tokenSecret = "glpat-TOKEN_SENTINEL_55"
	fakeJobID   = int64(700)
)

// --- fakes ---

type fakeGitLab struct {
	diffs     []gitlab.FileDiff
	files     map[string]string
	jobs      []gitlab.Job
	artifacts map[int64][]byte
	jobsErr   error
	diffsErr  error
	// plan flow
	statuses   []gitlab.CommitStatus
	statusSHAs []string
}

func (f *fakeGitLab) MergeRequestDiffs(context.Context, int) ([]gitlab.FileDiff, error) {
	return f.diffs, f.diffsErr
}

func (f *fakeGitLab) FileAtRef(_ context.Context, path, ref string) ([]byte, error) {
	if ref != headSHA {
		return nil, fmt.Errorf("unexpected ref %q", ref)
	}
	s, ok := f.files[path]
	if !ok {
		return nil, fmt.Errorf("file %s: %w", path, gitlab.ErrNotFound)
	}
	return []byte(s), nil
}

func (f *fakeGitLab) PipelineJobs(context.Context, int64) ([]gitlab.Job, error) {
	return f.jobs, f.jobsErr
}

func (f *fakeGitLab) JobArtifact(_ context.Context, jobID int64, artifactPath string) ([]byte, error) {
	b, ok := f.artifacts[jobID]
	if !ok || artifactPath != staticcheck.ArtifactName {
		return nil, fmt.Errorf("artifact: %w", gitlab.ErrNotFound)
	}
	return b, nil
}

func (f *fakeGitLab) MergeRequest(context.Context, int) (gitlab.MergeRequest, error) {
	return gitlab.MergeRequest{SHA: headSHA}, nil
}

func (f *fakeGitLab) MergeRequestPipelines(context.Context, int) ([]gitlab.Pipeline, error) {
	return nil, nil
}

func (f *fakeGitLab) CommitStatuses(_ context.Context, sha string) ([]gitlab.CommitStatus, error) {
	f.statusSHAs = append(f.statusSHAs, sha)
	return f.statuses, nil
}

// fakeStore is a stateful NoteStore so re-runs exercise the real upsert path.
type fakeStore struct {
	sha    string
	notes  []gitlab.Note
	nextID int64
	calls  int
}

func newFakeStore() *fakeStore { return &fakeStore{sha: headSHA, nextID: 100} }

func (s *fakeStore) CurrentUser(context.Context) (gitlab.User, error) {
	s.calls++
	return gitlab.User{ID: botID}, nil
}

// MergeRequest honours ctx like the real client, so publishing on a
// cancelled context fails.
func (s *fakeStore) MergeRequest(ctx context.Context, _ int) (gitlab.MergeRequest, error) {
	s.calls++
	if err := ctx.Err(); err != nil {
		return gitlab.MergeRequest{}, err
	}
	return gitlab.MergeRequest{SHA: s.sha}, nil
}

func (s *fakeStore) ListNotes(context.Context, int) ([]gitlab.Note, error) {
	s.calls++
	return append([]gitlab.Note(nil), s.notes...), nil
}

func (s *fakeStore) CreateNote(_ context.Context, _ int, body string) (gitlab.Note, error) {
	s.calls++
	s.nextID++
	n := gitlab.Note{ID: s.nextID, Body: body, AuthorID: botID}
	s.notes = append(s.notes, n)
	return n, nil
}

func (s *fakeStore) UpdateNote(_ context.Context, _ int, id int64, body string) error {
	s.calls++
	for i := range s.notes {
		if s.notes[i].ID == id {
			s.notes[i].Body = body
			return nil
		}
	}
	return gitlab.ErrNotFound
}

func (s *fakeStore) DeleteNote(_ context.Context, _ int, id int64) error {
	s.calls++
	for i := range s.notes {
		if s.notes[i].ID == id {
			s.notes = append(s.notes[:i], s.notes[i+1:]...)
			return nil
		}
	}
	return gitlab.ErrNotFound
}

func (s *fakeStore) body(t *testing.T) string {
	t.Helper()
	if len(s.notes) != 1 {
		t.Fatalf("notes = %d, want 1", len(s.notes))
	}
	return s.notes[0].Body
}

type fakeMCP struct{ calls []string }

func (m *fakeMCP) Call(_ context.Context, name string, _ map[string]any) (string, error) {
	m.calls = append(m.calls, name)
	if name == tfdocs.ToolLatestProviderVersion {
		return "6.10.0", nil
	}
	return "", fmt.Errorf("fake mcp: unexpected tool %s", name)
}

type fakeDocs struct {
	calls int
	err   error
}

func (d *fakeDocs) Lookup(_ context.Context, _ tfmcp.Caller, p model.ProviderRef, resourceType string,
	_ []string, _ int) (tfdocs.Excerpt, error) {
	d.calls++
	if d.err != nil {
		return tfdocs.Excerpt{}, d.err
	}
	url := "https://registry.terraform.io/providers/" + p.Namespace + "/" + p.Name + "/" + p.Version + "/docs/resources/" + resourceType
	return tfdocs.Excerpt{Text: docSecret + " text", DocURL: url}, nil
}

type fakeNarrator struct {
	calls    int
	in       llm.ReviewInput
	findings []model.Finding
	err      error
	// plan flow
	risksIn    []model.PlanSummary
	blocksIn   [][]string
	risks      []string
	failure    string
	risksErr   error
	failureErr error
	risksHook  func() // runs inside PlanRisks, e.g. to cancel the run
}

func (n *fakeNarrator) ReviewFindings(_ context.Context, in llm.ReviewInput) ([]model.Finding, error) {
	n.calls++
	n.in = in
	return n.findings, n.err
}

func (n *fakeNarrator) PlanRisks(_ context.Context, s model.PlanSummary) ([]string, error) {
	n.risksIn = append(n.risksIn, s)
	if n.risksHook != nil {
		n.risksHook()
	}
	return n.risks, n.risksErr
}

func (n *fakeNarrator) PlanFailure(_ context.Context, blocks []string) (string, error) {
	n.blocksIn = append(n.blocksIn, blocks)
	return n.failure, n.failureErr
}

// --- fixtures ---

const mainTF = `resource "google_storage_bucket" "logs" {
  name     = "` + diffSecret + `"
  location = "EU"
}
`

const versionsTF = `terraform {
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 6.0"
    }
  }
}
`

const lockHCL = `provider "registry.terraform.io/hashicorp/google" {
  version = "6.8.0"
}
`

const mainDiff = "@@ -1,3 +1,3 @@\n resource \"google_storage_bucket\" \"logs\" {\n-  name     = \"old\"\n+  name     = \"" + diffSecret + "\"\n   location = \"EU\"\n"

const artifactJSON = `{"version":1,"results":[{"tool":"fmt","passed":true,"summary":"ok"}]}`

type harness struct {
	gl    *fakeGitLab
	store *fakeStore
	mcp   *fakeMCP
	docs  *fakeDocs
	nar   *fakeNarrator
	logs  *bytes.Buffer
}

func newHarness() *harness {
	return &harness{
		gl: &fakeGitLab{
			diffs: []gitlab.FileDiff{{OldPath: "env/main.tf", NewPath: "env/main.tf", Diff: mainDiff}},
			files: map[string]string{
				"env/main.tf":             mainTF,
				"env/versions.tf":         versionsTF,
				"env/.terraform.lock.hcl": lockHCL,
			},
			jobs:      []gitlab.Job{{ID: fakeJobID, Name: staticcheck.JobName}},
			artifacts: map[int64][]byte{fakeJobID: []byte(artifactJSON)},
		},
		store: newFakeStore(),
		mcp:   &fakeMCP{},
		docs:  &fakeDocs{},
		nar: &fakeNarrator{findings: []model.Finding{
			{Resource: "google_storage_bucket.logs", Severity: "중간", Message: "버킷 이름 변경은 재생성을 유발합니다"},
		}},
		logs: &bytes.Buffer{},
	}
}

func (h *harness) deps() Deps {
	return Deps{
		GitLab:   h.gl,
		Notes:    h.store,
		MCP:      h.mcp,
		Narrator: h.nar,
		Docs:     h.docs,
		Config: config.Config{Limits: config.Limits{
			DiffBytes: 10000, DocsBytes: 4000, LLMOutputBytes: 4000,
		}},
		Log: slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
}

func (h *harness) run(t *testing.T) error {
	t.Helper()
	return RunCode(context.Background(), h.deps(), input.Input{MRIID: 7, SHA: headSHA, PipelineID: 900})
}

func mustContain(t *testing.T, body string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("body lacks %q:\n%s", w, body)
		}
	}
}

// --- tests ---

func TestRunCode_NormalPathCreatesOneNoteAndRerunKeepsOne(t *testing.T) {
	h := newHarness()
	if err := h.run(t); err != nil {
		t.Fatalf("RunCode: %v", err)
	}
	body := h.store.body(t)
	mustContain(t, body,
		"<!-- tf-review:v1:code:- -->",
		"버킷 이름 변경은 재생성을 유발합니다",
		"https://registry.terraform.io/providers/hashicorp/google/6.8.0/docs/resources/google_storage_bucket",
		"_根拠: lock ファイルの provider バージョンに対応する Terraform 公式ドキュメント。_",
		"| fmt |",
	)
	if strings.Contains(body, "変更が大きいため、一部のみレビューしました。") {
		t.Errorf("body unexpectedly marked truncated")
	}
	if err := h.run(t); err != nil {
		t.Fatalf("RunCode rerun: %v", err)
	}
	h.store.body(t)
	if h.store.notes[0].ID != 101 {
		t.Errorf("note ID = %d after rerun, want 101", h.store.notes[0].ID)
	}
}

func TestRunCode_ReviewInputContents(t *testing.T) {
	h := newHarness()
	if err := h.run(t); err != nil {
		t.Fatalf("RunCode: %v", err)
	}
	in := h.nar.in
	if len(in.Resources) != 1 {
		t.Fatalf("resources = %+v", in.Resources)
	}
	r := in.Resources[0]
	if r.Address != "google_storage_bucket.logs" || r.File != "env/main.tf" {
		t.Errorf("resource = %+v", r)
	}
	if r.DiffExcerpt != mainDiff || !strings.Contains(r.DocExcerpt, docSecret) {
		t.Errorf("excerpts not filled: %+v", r)
	}
	if len(in.Checks) != 4 || in.Checks[0].Tool != "fmt" {
		t.Errorf("checks = %+v", in.Checks)
	}
	if len(h.mcp.calls) != 0 {
		t.Errorf("lock basis should not call MCP, got %v", h.mcp.calls)
	}
}

const netModuleTF = "module \"net\" {\n  source  = \"terraform-google-modules/network/google\"\n  version = \"9.0.0\"\n}\n"

const netModuleDiff = "@@ -0,0 +1,4 @@\n+module \"net\" {\n+  source  = \"terraform-google-modules/network/google\"\n+  version = \"9.0.0\"\n+}\n"

func TestRunCode_ModulesGoToModuleContext(t *testing.T) {
	h := newHarness()
	h.gl.files["env/net.tf"] = netModuleTF
	h.gl.diffs = append(h.gl.diffs, gitlab.FileDiff{OldPath: "env/net.tf", NewPath: "env/net.tf", NewFile: true, Diff: netModuleDiff})
	if err := h.run(t); err != nil {
		t.Fatalf("RunCode: %v", err)
	}
	want := llm.ModuleContext{Name: "net", Source: "terraform-google-modules/network/google", Version: "9.0.0"}
	if len(h.nar.in.Modules) != 1 || h.nar.in.Modules[0] != want {
		t.Errorf("modules = %+v", h.nar.in.Modules)
	}
}

// Modules have no documentation lookup, so a modules-only MR is not reviewed.
func TestRunCode_ModulesOnlySkipsLLM(t *testing.T) {
	h := newHarness()
	h.gl.files["env/main.tf"] = netModuleTF
	h.gl.diffs[0].Diff = netModuleDiff
	if err := h.run(t); err != nil {
		t.Fatalf("RunCode: %v", err)
	}
	if h.nar.calls != 0 {
		t.Errorf("LLM called for a modules-only MR")
	}
	mustContain(t, h.store.body(t), "ドキュメントの根拠があるリソースがないため、AI レビューは行いませんでした。", "_公式ドキュメントの根拠は使用していません。_")
}

func TestRunCode_NoTFChangesPostsNothing(t *testing.T) {
	h := newHarness()
	h.gl.diffs = []gitlab.FileDiff{
		{OldPath: "env/prod.tfvars", NewPath: "env/prod.tfvars", Diff: "@@ -1 +1 @@\n-a = 1\n+a = 2\n"},
		{OldPath: "README.md", NewPath: "README.md", Diff: "@@ -1 +1 @@\n-x\n+y\n"},
	}
	if err := h.run(t); err != nil {
		t.Fatalf("RunCode: %v", err)
	}
	if h.store.calls != 0 || h.nar.calls != 0 {
		t.Errorf("notes calls = %d, narrator calls = %d; want 0, 0", h.store.calls, h.nar.calls)
	}
}

func TestRunCode_CollapsedNewFileIsMarkedTruncated(t *testing.T) {
	h := newHarness()
	h.gl.files["env/new.tf"] = "resource \"google_storage_bucket\" \"new\" {\n  location = \"EU\"\n}\n"
	h.gl.diffs = append(h.gl.diffs, gitlab.FileDiff{OldPath: "env/new.tf", NewPath: "env/new.tf", NewFile: true})
	if err := h.run(t); err != nil {
		t.Fatalf("RunCode: %v", err)
	}
	mustContain(t, h.store.body(t), "> 変更が大きいため、一部のみレビューしました。")
}

func TestRunCode_AllDiffsCollapsedPostsTruncationWithoutLLM(t *testing.T) {
	h := newHarness()
	h.gl.diffs = []gitlab.FileDiff{{OldPath: "env/main.tf", NewPath: "env/main.tf"}}
	if err := h.run(t); err != nil {
		t.Fatalf("RunCode: %v", err)
	}
	if h.nar.calls != 0 {
		t.Errorf("narrator called %d times, want 0", h.nar.calls)
	}
	mustContain(t, h.store.body(t), "> 変更が大きいため、一部のみレビューしました。", "指摘事項はありません")
}

// twoDirHarness has the same address in dirs a/ and b/ with different
// constraints and no lock files: a resolves (6.10.0 satisfies ~> 6.0), b does
// not (~> 5.0).
func twoDirHarness() *harness {
	h := newHarness()
	res := "resource \"google_storage_bucket\" \"x\" {\n  location = \"EU\"\n}\n"
	diff := "@@ -0,0 +1,3 @@\n+resource \"google_storage_bucket\" \"x\" {\n+  location = \"EU\"\n+}\n"
	h.gl.files = map[string]string{
		"a/main.tf": res, "a/versions.tf": versionsTF,
		"b/main.tf": res, "b/versions.tf": strings.Replace(versionsTF, "~> 6.0", "~> 5.0", 1),
	}
	h.gl.diffs = []gitlab.FileDiff{
		{OldPath: "a/main.tf", NewPath: "a/main.tf", NewFile: true, Diff: diff},
		{OldPath: "b/main.tf", NewPath: "b/main.tf", NewFile: true, Diff: diff},
	}
	h.nar.findings = []model.Finding{
		{Resource: "a/google_storage_bucket.x", Severity: "낮음", Message: "A_FINDING"},
	}
	return h
}

func lineWith(t *testing.T, body, needle string) string {
	t.Helper()
	for _, l := range strings.Split(body, "\n") {
		if strings.Contains(l, needle) {
			return l
		}
	}
	t.Fatalf("no line with %q:\n%s", needle, body)
	return ""
}

func TestRunCode_ProviderDeclsAndDocsArePerDirectory(t *testing.T) {
	h := twoDirHarness()
	if err := h.run(t); err != nil {
		t.Fatalf("RunCode: %v", err)
	}
	// b has no doc evidence: it is not sent to the LLM but listed as skipped.
	res := h.nar.in.Resources
	if len(res) != 1 || res[0].Address != "a/google_storage_bucket.x" || !strings.Contains(res[0].DocExcerpt, docSecret) {
		t.Fatalf("resources = %+v", res)
	}
	body := h.store.body(t)
	if !strings.Contains(lineWith(t, body, "A_FINDING"), "/hashicorp/google/6.10.0/") {
		t.Errorf("a finding lacks its DocRef:\n%s", body)
	}
	mustContain(t, body, "ドキュメントの根拠がないためレビュー対象外: `b/google_storage_bucket.x`", "_根拠: バージョン制約に基づく")
}

func TestRunCode_NoDocEvidenceSkipsLLMAndListsResources(t *testing.T) {
	for name, mutate := range map[string]func(h *harness){
		"lookup fails": func(h *harness) { h.docs.err = tfdocs.ErrDocNotFound },
		"no version": func(h *harness) {
			delete(h.gl.files, "env/.terraform.lock.hcl")
			h.gl.files["env/versions.tf"] = strings.Replace(versionsTF, "~> 6.0", "~> 5.0", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness()
			mutate(h)
			if err := h.run(t); err != nil {
				t.Fatalf("RunCode: %v", err)
			}
			if h.nar.calls != 0 {
				t.Errorf("LLM called %d times without doc evidence", h.nar.calls)
			}
			mustContain(t, h.store.body(t),
				"ドキュメントの根拠があるリソースがないため、AI レビューは行いませんでした。",
				"ドキュメントの根拠がないためレビュー対象外: `google_storage_bucket.logs`",
				"| fmt | 成功 | ok |",
				"_公式ドキュメントの根拠は使用していません。_",
			)
		})
	}
}

func TestRunCode_LLMTextIsCappedAtLLMOutputBytes(t *testing.T) {
	h := newHarness()
	h.nar.findings = []model.Finding{{Resource: "google_storage_bucket.logs", Severity: "info", Message: strings.Repeat("가", 3000)}}
	if err := h.run(t); err != nil {
		t.Fatalf("RunCode: %v", err)
	}
	line := lineWith(t, h.store.body(t), "가가가")
	if strings.Count(line, "가")*3 > 4000 || !strings.Contains(line, "…") {
		t.Errorf("finding not capped at 4000 bytes: %d runes", strings.Count(line, "가"))
	}
}

func TestReviewAddresses_SameDirUsesFile(t *testing.T) {
	rcs := []hclscan.ResourceChange{
		{File: "a/one.tf", Type: "t_x", Name: "n"},
		{File: "a/two.tf", Type: "t_x", Name: "n"},
		{File: "a/one.tf", Type: "t_y", Name: "n"},
	}
	got := reviewAddresses(rcs)
	want := []string{"a/one.tf:t_x.n", "a/two.tf:t_x.n", "t_y.n"}
	if !slices.Equal(got, want) {
		t.Errorf("addresses = %v, want %v", got, want)
	}
}

func TestRunCode_LLMErrorWithStaleHeadReturnsLLMError(t *testing.T) {
	h := newHarness()
	h.nar.err = errors.New("model unavailable")
	h.store.sha = "ffffffffffffffffffffffffffffffffffffffff"
	if err := h.run(t); !errors.Is(err, h.nar.err) {
		t.Fatalf("err = %v, want wrapped LLM error", err)
	}
	if len(h.store.notes) != 0 {
		t.Errorf("stale run wrote notes")
	}
}

func TestRunCode_DiffOverCapIsMarkedTruncated(t *testing.T) {
	h := newHarness()
	d := h.deps()
	d.Config.Limits.DiffBytes = 20
	if err := RunCode(context.Background(), d, input.Input{MRIID: 7, SHA: headSHA, PipelineID: 900}); err != nil {
		t.Fatalf("RunCode: %v", err)
	}
	mustContain(t, h.store.body(t), "> 変更が大きいため、一部のみレビューしました。")
	if !h.nar.in.DiffTruncated || len(h.nar.in.Resources[0].DiffExcerpt) > 20 {
		t.Errorf("review input not capped: %+v", h.nar.in)
	}
}

func TestRunCode_NonTFDiffsDoNotCountTowardCap(t *testing.T) {
	h := newHarness()
	h.gl.diffs = append(h.gl.diffs, gitlab.FileDiff{OldPath: "README.md", NewPath: "README.md",
		Diff: strings.Repeat("x", 20000)})
	if err := h.run(t); err != nil {
		t.Fatalf("RunCode: %v", err)
	}
	if h.nar.in.DiffTruncated {
		t.Errorf("non-.tf diff should not trigger truncation")
	}
}

func TestRunCode_CollapsedModifiedDiffIsMarkedTruncated(t *testing.T) {
	h := newHarness()
	h.gl.files["env/other.tf"] = "resource \"google_storage_bucket\" \"other\" {\n  location = \"EU\"\n}\n"
	h.gl.diffs = append(h.gl.diffs, gitlab.FileDiff{OldPath: "env/other.tf", NewPath: "env/other.tf"})
	if err := h.run(t); err != nil {
		t.Fatalf("RunCode: %v", err)
	}
	mustContain(t, h.store.body(t), "> 変更が大きいため、一部のみレビューしました。")
}

func TestRunCode_VersionNoneShowsNoDocEvidence(t *testing.T) {
	h := newHarness()
	delete(h.gl.files, "env/.terraform.lock.hcl")
	h.gl.files["env/versions.tf"] = strings.Replace(versionsTF, "~> 6.0", "~> 5.0", 1)
	if err := h.run(t); err != nil {
		t.Fatalf("RunCode: %v", err)
	}
	mustContain(t, h.store.body(t), "_公式ドキュメントの根拠は使用していません。_")
	if h.docs.calls != 0 {
		t.Errorf("docs looked up without a version")
	}
}

func TestRunCode_ConstraintBasisUsesLatestVersion(t *testing.T) {
	h := newHarness()
	delete(h.gl.files, "env/.terraform.lock.hcl")
	if err := h.run(t); err != nil {
		t.Fatalf("RunCode: %v", err)
	}
	mustContain(t, h.store.body(t), "_根拠: バージョン制約に基づく", "/hashicorp/google/6.10.0/")
}

func TestRunCode_LookupFailureFallsBackToNone(t *testing.T) {
	h := newHarness()
	h.docs.err = tfdocs.ErrDocNotFound
	if err := h.run(t); err != nil {
		t.Fatalf("RunCode: %v", err)
	}
	body := h.store.body(t)
	mustContain(t, body, "_公式ドキュメントの根拠は使用していません。_")
	if strings.Contains(body, "registry.terraform.io") {
		t.Errorf("failed lookup should leave no DocRef:\n%s", body)
	}
	mustContain(t, h.logs.String(), "google_storage_bucket.logs")
}

func TestRunCode_LLMErrorPostsAnalysisFailedAndReturnsError(t *testing.T) {
	h := newHarness()
	h.nar.err = errors.New("model unavailable")
	err := h.run(t)
	if err == nil || !errors.Is(err, h.nar.err) {
		t.Fatalf("err = %v, want wrapped LLM error", err)
	}
	mustContain(t, h.store.body(t), "> 分析失敗:", "所見を生成できませんでした。")
}

func TestRunCode_NoStaticCheckArtifact(t *testing.T) {
	for name, mutate := range map[string]func(*fakeGitLab){
		"no job":      func(g *fakeGitLab) { g.jobs = nil },
		"no artifact": func(g *fakeGitLab) { g.artifacts = nil },
		"bad json":    func(g *fakeGitLab) { g.artifacts[fakeJobID] = []byte("{") },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness()
			mutate(h.gl)
			if err := h.run(t); err != nil {
				t.Fatalf("RunCode: %v", err)
			}
			mustContain(t, h.store.body(t), "静的チェックの結果はありません")
		})
	}
}

func TestRunCode_PipelineJobsErrorIsReturned(t *testing.T) {
	h := newHarness()
	h.gl.jobsErr = errors.New("gitlab 500")
	if err := h.run(t); !errors.Is(err, h.gl.jobsErr) {
		t.Fatalf("err = %v, want wrapped jobs error", err)
	}
	assertCodeFailed(t, h, false)
}

// assertCodeFailed checks the analysis-failed code note: static checks when
// withChecks, no findings, no error text, and that the LLM was not called.
func assertCodeFailed(t *testing.T, h *harness, withChecks bool) {
	t.Helper()
	body := h.store.body(t)
	mustContain(t, body, "> 分析失敗:", "所見を生成できませんでした。")
	if withChecks {
		mustContain(t, body, "| fmt | 成功 | ok |")
	} else {
		mustContain(t, body, "静的チェックの結果はありません")
	}
	if h.nar.calls != 0 {
		t.Errorf("LLM called %d times", h.nar.calls)
	}
	for _, s := range []string{"boom", "gitlab 500", "Argument", "Unclosed"} {
		if strings.Contains(body, s) {
			t.Errorf("note carries error text %q:\n%s", s, body)
		}
	}
}

func TestRunCode_DiffFetchErrorPostsAnalysisFailed(t *testing.T) {
	h := newHarness()
	h.gl.diffsErr = errors.New("diffs boom")
	if err := h.run(t); !errors.Is(err, h.gl.diffsErr) {
		t.Fatalf("err = %v, want wrapped diff error", err)
	}
	assertCodeFailed(t, h, false)
}

// A single HCL syntax error must not hide the static checks.
func TestRunCode_ScanErrorPostsAnalysisFailedWithChecks(t *testing.T) {
	h := newHarness()
	h.gl.files["env/main.tf"] = "resource \"google_storage_bucket\" \"logs\" {\n  name = \n"
	if err := h.run(t); err == nil {
		t.Fatal("want scan error")
	}
	assertCodeFailed(t, h, true)
}

func TestRunCode_ProviderDeclsErrorPostsAnalysisFailedWithChecks(t *testing.T) {
	h := newHarness()
	h.gl.files["env/versions.tf"] = "terraform {\n  required_providers {\n"
	if err := h.run(t); err == nil {
		t.Fatal("want provider declarations error")
	}
	assertCodeFailed(t, h, true)
}

func TestRunCode_CancelledRunStillPostsNotice(t *testing.T) {
	h := newHarness()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.nar.err = context.Canceled
	err := RunCode(ctx, h.deps(), input.Input{MRIID: 7, SHA: headSHA, PipelineID: 900})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	mustContain(t, h.store.body(t), "> 分析失敗:")
}

func TestRunCode_PrivateProviderMakesNoMCPCalls(t *testing.T) {
	h := newHarness()
	delete(h.gl.files, "env/.terraform.lock.hcl")
	h.gl.files["env/versions.tf"] = strings.Replace(versionsTF, "hashicorp/google", "registry.example.com/acme/google", 1)
	if err := h.run(t); err != nil {
		t.Fatalf("RunCode: %v", err)
	}
	if len(h.mcp.calls) != 0 || h.docs.calls != 0 {
		t.Errorf("mcp calls = %v, docs calls = %d; want none", h.mcp.calls, h.docs.calls)
	}
	mustContain(t, h.store.body(t), "_公式ドキュメントの根拠は使用していません。_")
}

func TestRunCode_StaleHeadReturnsNil(t *testing.T) {
	h := newHarness()
	h.store.sha = "ffffffffffffffffffffffffffffffffffffffff"
	if err := h.run(t); err != nil {
		t.Fatalf("RunCode: %v, want nil on stale head", err)
	}
	if len(h.store.notes) != 0 {
		t.Errorf("stale run wrote notes")
	}
}

func TestRunCode_LogsCarryNoPayloads(t *testing.T) {
	h := newHarness()
	h.gl.artifacts[fakeJobID] = []byte("{")
	h.docs.err = fmt.Errorf("lookup: %w", tfdocs.ErrDocNotFound)
	h.nar.err = errors.New("model unavailable")
	_ = h.run(t)
	h2 := newHarness()
	_ = h2.run(t)
	logs := h.logs.String() + h2.logs.String()
	if logs == "" {
		t.Fatal("expected some logs")
	}
	for _, s := range []string{diffSecret, docSecret, tokenSecret, "location"} {
		if strings.Contains(logs, s) {
			t.Errorf("logs contain %q:\n%s", s, logs)
		}
	}
}

// Removed resources get no doc lookup and no LLM review; they are listed as
// deleted, not as lacking doc evidence.
func TestRunCode_RemovedResourceListedSeparately(t *testing.T) {
	h := newHarness()
	h.gl.diffs = append(h.gl.diffs, gitlab.FileDiff{OldPath: "env/gone.tf", NewPath: "env/gone.tf", DeletedFile: true,
		Diff: "@@ -1,3 +0,0 @@\n-resource \"google_pubsub_topic\" \"t\" {\n-  name = \"t\"\n-}\n"})
	if err := h.run(t); err != nil {
		t.Fatalf("RunCode: %v", err)
	}
	if res := h.nar.in.Resources; len(res) != 1 || res[0].Address != "google_storage_bucket.logs" {
		t.Fatalf("LLM resources = %+v", res)
	}
	body := h.store.body(t)
	mustContain(t, body, "削除されたリソース (ドキュメント照会の対象外): `google_pubsub_topic.t`")
	if strings.Contains(body, "レビュー対象外") {
		t.Errorf("removed resource listed as doc-less:\n%s", body)
	}
	if h.docs.calls != 1 {
		t.Errorf("doc lookups = %d, want 1 (removed resource not looked up)", h.docs.calls)
	}
}
