package review

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/terraform-agent/internal/gitlab"
	"github.com/terraform-agent/internal/input"
	"github.com/terraform-agent/internal/plan"
	"github.com/terraform-agent/internal/tfmcp"
)

const (
	allowedProject  = int64(4242)
	otherProject    = int64(999)
	runPipelineID   = 900
	payloadSentinel = "DIAG_PAYLOAD_SENTINEL_31"
	diagErrSentinel = "DIAG_ERR_SENTINEL_77"
)

// --- run fakes ---

// runGitLab overrides the entrypoint-relevant reads of fakeGitLab.
type runGitLab struct {
	*fakeGitLab
	mr           gitlab.MergeRequest
	mrErr        error
	pipelines    []gitlab.Pipeline
	pipelinesErr error
	diffsErr     error
	diffCalls    int
}

func (g *runGitLab) MergeRequest(context.Context, int) (gitlab.MergeRequest, error) {
	return g.mr, g.mrErr
}

func (g *runGitLab) MergeRequestPipelines(context.Context, int) ([]gitlab.Pipeline, error) {
	return g.pipelines, g.pipelinesErr
}

func (g *runGitLab) MergeRequestDiffs(ctx context.Context, iid int) ([]gitlab.FileDiff, error) {
	g.diffCalls++
	if g.diffsErr != nil {
		return nil, g.diffsErr
	}
	return g.fakeGitLab.MergeRequestDiffs(ctx, iid)
}

type fakeTools struct {
	calls int
	err   error
	block bool
}

func (f *fakeTools) VerifyTools(ctx context.Context) error {
	f.calls++
	if f.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return f.err
}

// diagMCP answers the diagnostic tools and forwards everything else.
type diagMCP struct {
	inner   tfmcp.Caller
	diagErr error
	diag    []string
}

func (m *diagMCP) Call(ctx context.Context, name string, args map[string]any) (string, error) {
	if slices.Contains(DiagnosticTools, name) {
		m.diag = append(m.diag, name)
		return payloadSentinel, m.diagErr
	}
	return m.inner.Call(ctx, name, args)
}

type runHarness struct {
	*planHarness
	rgl   *runGitLab
	tools *fakeTools
	dm    *diagMCP
}

func newRunHarness() *runHarness {
	h := newPlanHarness("ws-a")
	h.addFinished("ws-a", "run-1", "plan-1")
	rgl := &runGitLab{
		fakeGitLab: h.gl,
		mr: gitlab.MergeRequest{IID: 7, SHA: headSHA,
			SourceProjectID: allowedProject, TargetProjectID: allowedProject},
		pipelines: []gitlab.Pipeline{{ID: 899, SHA: headSHA}, {ID: runPipelineID, SHA: headSHA}},
	}
	return &runHarness{planHarness: h, rgl: rgl, tools: &fakeTools{}, dm: &diagMCP{inner: h.pm}}
}

func (h *runHarness) runDeps() Deps {
	d := h.planDeps()
	d.GitLab = h.rgl
	d.MCP = h.dm
	d.Tools = h.tools
	d.Config.AllowedGitlabProjectID = allowedProject
	return d
}

func (h *runHarness) runAll(ctx context.Context) error {
	return Run(ctx, h.runDeps(), input.Input{MRIID: 7, SHA: headSHA, PipelineID: runPipelineID})
}

// assertNothingRan checks that no MCP, flow or publishing work happened.
func (h *runHarness) assertNothingRan(t *testing.T) {
	t.Helper()
	if h.tools.calls != 0 || len(h.dm.diag) != 0 || len(h.pm.calls) != 0 {
		t.Errorf("MCP used: verify=%d diag=%v calls=%d", h.tools.calls, h.dm.diag, len(h.pm.calls))
	}
	if h.rgl.diffCalls != 0 || len(h.gl.statusSHAs) != 0 {
		t.Errorf("flows ran: diffs=%d statuses=%d", h.rgl.diffCalls, len(h.gl.statusSHAs))
	}
	if h.store.calls != 0 || h.nar.calls != 0 || h.llmCalls() != 0 {
		t.Errorf("published or LLM called: store=%d llm=%d/%d", h.store.calls, h.nar.calls, h.llmCalls())
	}
}

// --- tests ---

func TestRun_HappyPathRunsDiagnosticsAndBothFlows(t *testing.T) {
	h := newRunHarness()
	if err := h.runAll(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if h.tools.calls != 1 {
		t.Errorf("VerifyTools calls = %d, want 1", h.tools.calls)
	}
	if !slices.Equal(h.dm.diag, DiagnosticTools) {
		t.Errorf("diagnostics = %v, want %v", h.dm.diag, DiagnosticTools)
	}
	if len(h.store.notes) != 2 {
		t.Fatalf("notes = %d, want code + plan", len(h.store.notes))
	}
	h.noteFor(t, "ws-a")
	if strings.Contains(h.logs.String(), payloadSentinel) {
		t.Errorf("diagnostic payload logged:\n%s", h.logs.String())
	}
}

func TestRun_MRFromDisallowedProjectFailsBeforeAnything(t *testing.T) {
	h := newRunHarness()
	h.rgl.mr.SourceProjectID, h.rgl.mr.TargetProjectID = otherProject, otherProject
	if err := h.runAll(context.Background()); err == nil {
		t.Fatal("want error for disallowed project")
	}
	h.assertNothingRan(t)
}

func TestRun_ForkMRFails(t *testing.T) {
	h := newRunHarness()
	h.rgl.mr.SourceProjectID = otherProject
	if err := h.runAll(context.Background()); err == nil {
		t.Fatal("want error for fork MR")
	}
	h.assertNothingRan(t)
}

// The project check runs before the stale-SHA check, so a disallowed MR can
// never be silently skipped.
func TestRun_DisallowedProjectWithStaleSHAStillFails(t *testing.T) {
	h := newRunHarness()
	h.rgl.mr.TargetProjectID = otherProject
	h.rgl.mr.SHA = strings.Repeat("f", 40)
	if err := h.runAll(context.Background()); err == nil {
		t.Fatal("want error, got nil")
	}
	h.assertNothingRan(t)
}

func TestRun_SHAMismatchReturnsNilAndRunsNothing(t *testing.T) {
	h := newRunHarness()
	h.rgl.mr.SHA = strings.Repeat("f", 40)
	h.rgl.pipelinesErr = errors.New("pipelines must not be read")
	if err := h.runAll(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	h.assertNothingRan(t)
	if !strings.Contains(h.logs.String(), "newer push") {
		t.Errorf("log lacks newer push notice:\n%s", h.logs.String())
	}
}

func TestRun_PipelineNotOfThisMRFails(t *testing.T) {
	cases := map[string][]gitlab.Pipeline{
		"absent":    {{ID: 899, SHA: headSHA}},
		"wrong SHA": {{ID: runPipelineID, SHA: strings.Repeat("e", 40)}},
		"none":      nil,
	}
	for name, pipelines := range cases {
		t.Run(name, func(t *testing.T) {
			h := newRunHarness()
			h.rgl.pipelines = pipelines
			if err := h.runAll(context.Background()); err == nil {
				t.Fatal("want error")
			}
			h.assertNothingRan(t)
		})
	}
}

func TestRun_GitLabReadErrorsFail(t *testing.T) {
	boom := errors.New("gitlab down")
	for name, set := range map[string]func(*runGitLab){
		"merge request": func(g *runGitLab) { g.mrErr = boom },
		"pipelines":     func(g *runGitLab) { g.pipelinesErr = boom },
	} {
		t.Run(name, func(t *testing.T) {
			h := newRunHarness()
			set(h.rgl)
			if err := h.runAll(context.Background()); !errors.Is(err, boom) {
				t.Fatalf("err = %v, want wrapping %v", err, boom)
			}
			h.assertNothingRan(t)
		})
	}
}

func TestRun_VerifyToolsFailureFails(t *testing.T) {
	h := newRunHarness()
	verifyErr := errors.New("allowlisted tools missing")
	h.tools.err = verifyErr
	if err := h.runAll(context.Background()); !errors.Is(err, verifyErr) {
		t.Fatalf("err = %v, want wrapping %v", err, verifyErr)
	}
	if len(h.dm.diag) != 0 || h.rgl.diffCalls != 0 || h.store.calls != 0 {
		t.Errorf("work after VerifyTools failure: diag=%v diffs=%d store=%d", h.dm.diag, h.rgl.diffCalls, h.store.calls)
	}
}

func TestRun_DiagnosticFailureIsIgnoredAndNotLogged(t *testing.T) {
	h := newRunHarness()
	h.dm.diagErr = errors.New(diagErrSentinel)
	if err := h.runAll(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(h.store.notes) != 2 {
		t.Errorf("notes = %d, want code + plan", len(h.store.notes))
	}
	logs := h.logs.String()
	for _, name := range DiagnosticTools {
		if !strings.Contains(logs, name) {
			t.Errorf("log lacks diagnostic %s:\n%s", name, logs)
		}
	}
	for _, leak := range []string{diagErrSentinel, payloadSentinel} {
		if strings.Contains(logs, leak) {
			t.Errorf("log contains %q:\n%s", leak, logs)
		}
	}
}

func TestRun_CodeFailureStillRunsPlanAndFails(t *testing.T) {
	h := newRunHarness()
	diffErr := errors.New("diffs boom")
	h.rgl.diffsErr = diffErr
	err := h.runAll(context.Background())
	if !errors.Is(err, diffErr) {
		t.Fatalf("err = %v, want wrapping %v", err, diffErr)
	}
	if len(h.pm.callsTo(plan.ToolGetRunDetails)) == 0 {
		t.Error("plan flow did not run")
	}
	h.noteFor(t, "ws-a")
}

// Preflight must work before MCP and the LLM exist (main runs it first).
func TestPreflight_NeedsOnlyGitLabConfigAndLog(t *testing.T) {
	h := newRunHarness()
	full := h.runDeps()
	d := Deps{GitLab: h.rgl, Config: full.Config, Log: full.Log}
	in := input.Input{MRIID: 7, SHA: headSHA, PipelineID: runPipelineID}
	if ok, err := Preflight(context.Background(), d, in); !ok || err != nil {
		t.Fatalf("Preflight = %v, %v; want true, nil", ok, err)
	}
	h.rgl.mr.SHA = strings.Repeat("f", 40)
	if ok, err := Preflight(context.Background(), d, in); ok || err != nil {
		t.Fatalf("stale: Preflight = %v, %v; want false, nil", ok, err)
	}
	h.rgl.mr.SourceProjectID = otherProject
	if ok, err := Preflight(context.Background(), d, in); ok || err == nil {
		t.Fatalf("fork: Preflight = %v, %v; want false, error", ok, err)
	}
}

func TestRun_DeadlineCancelsRun(t *testing.T) {
	h := newRunHarness()
	h.tools.block = true
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := h.runAll(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if h.rgl.diffCalls != 0 || h.store.calls != 0 {
		t.Errorf("work after deadline: diffs=%d store=%d", h.rgl.diffCalls, h.store.calls)
	}
}
