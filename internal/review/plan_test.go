package review

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/terraform-agent/internal/config"
	"github.com/terraform-agent/internal/gitlab"
	"github.com/terraform-agent/internal/input"
	"github.com/terraform-agent/internal/model"
	"github.com/terraform-agent/internal/plan"
	"github.com/terraform-agent/internal/render"
)

const (
	tfOrg     = "acme"
	wsProd    = "net-prod"
	wsDev     = "net-dev"
	otherSHA  = "fedcba9876543210fedcba9876543210fedcba98"
	riskText  = "인스턴스 교체로 짧은 중단이 발생할 수 있습니다"
	failText  = "변수 값이 유효하지 않아 plan이 실패했습니다"
	runSecret = "RUN_DETAILS_SENTINEL_31"
)

// Secret sentinels live only in JSON values, never in attribute names.
var planSecrets = []string{"VAR_SECRET_1a", "PRIOR_SECRET_2b", "BEFORE_SECRET_3c", "AFTER_SECRET_4d", "PLANNED_SECRET_5e"}

const planJSONFixture = `{"format_version":"1.2",
"variables":{"db_password":{"value":"VAR_SECRET_1a"}},
"planned_values":{"root_module":{"resources":[{"address":"google_sql_user.app","values":{"password":"PLANNED_SECRET_5e"}}]}},
"prior_state":{"values":{"root_module":{"resources":[{"address":"google_sql_user.app","values":{"password":"PRIOR_SECRET_2b"}}]}}},
"resource_changes":[
 {"address":"google_sql_user.app","change":{"actions":["update"],"before":{"name":"app","password":"BEFORE_SECRET_3c"},"after":{"name":"app","password":"AFTER_SECRET_4d"},"after_unknown":{}}},
 {"address":"google_compute_instance.web","change":{"actions":["delete","create"],"before":{"machine_type":"e2-small"},"after":{"machine_type":"e2-medium"},"after_unknown":{"id":true}}}
]}`

var wantSummary = model.PlanSummary{
	Change: 1, Replace: 1,
	Targets: []model.Target{
		{Address: "google_compute_instance.web", Action: "replace", Attrs: []string{"id", "machine_type"}},
		{Address: "google_sql_user.app", Action: "update", Attrs: []string{"password"}},
	},
}

const errorBlock = "Error: Invalid value for variable\n\nThe given value is not valid."

const failedLogFixture = "Terraform v1.9.0\nexport TFE_TOKEN=" + tokenSecret + "\n" +
	"╷\n│ Error: Invalid value for variable\n│\n│ The given value is not valid.\n╵\n"

const noBlockLogFixture = "Terraform v1.9.0\nsomething odd happened " + tokenSecret + "\n"

// --- plan fakes ---

type mcpCall struct {
	name string
	args map[string]any
}

// planMCP serves run details by run ID and plan JSON/logs by plan ID.
type planMCP struct {
	runs     map[string]string
	planJSON map[string]string
	logs     map[string]string
	errs     map[string]error
	calls    []mcpCall
}

func newPlanMCP() *planMCP {
	return &planMCP{runs: map[string]string{}, planJSON: map[string]string{}, logs: map[string]string{}, errs: map[string]error{}}
}

func (m *planMCP) Call(_ context.Context, name string, args map[string]any) (string, error) {
	m.calls = append(m.calls, mcpCall{name: name, args: args})
	if err := m.errs[name]; err != nil {
		return "", err
	}
	var out string
	var ok bool
	switch name {
	case plan.ToolGetRunDetails:
		out, ok = m.runs[fmt.Sprint(args["run_id"])]
	case plan.ToolGetPlanJSON:
		out, ok = m.planJSON[fmt.Sprint(args["plan_id"])]
	case plan.ToolGetPlanLogs:
		out, ok = m.logs[fmt.Sprint(args["plan_id"])]
	}
	if !ok {
		return "", fmt.Errorf("fake mcp: unexpected call %s", name)
	}
	return out, nil
}

func (m *planMCP) callsTo(name string) []mcpCall {
	var out []mcpCall
	for _, c := range m.calls {
		if c.name == name {
			out = append(out, c)
		}
	}
	return out
}

type stepClock struct{ now time.Time }

func (c *stepClock) Now() time.Time { return c.now }

func (c *stepClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.now = c.now.Add(d)
	return nil
}

// runDetails mirrors the PROVISIONAL JSON:API run payload plan.Wait parses.
func runDetails(runID, status, planID, sha, wsName, org string) string {
	return `{"data":{"id":"` + runID + `","attributes":{"status":"` + status + `","commit-sha":"` + sha +
		`","message":"` + runSecret + `"},"relationships":{"plan":{"data":{"id":"` + planID + `","type":"plans"}},` +
		`"workspace":{"data":{"id":"ws-x","type":"workspaces"}}}},` +
		`"included":[{"id":"ws-x","type":"workspaces","attributes":{"name":"` + wsName + `"},` +
		`"relationships":{"organization":{"data":{"id":"` + org + `"}}}}]}`
}

func runStatus(ws, runID string) gitlab.CommitStatus {
	return gitlab.CommitStatus{
		Name: "Terraform Cloud/" + tfOrg + "/" + ws, Status: "success",
		TargetURL: "https://app.terraform.io/app/" + tfOrg + "/" + ws + "/runs/" + runID,
	}
}

func runURLOf(ws, runID string) string {
	return "https://app.terraform.io/app/" + tfOrg + "/workspaces/" + ws + "/runs/" + runID
}

type planHarness struct {
	*harness
	pm         *planMCP
	workspaces []string
}

func newPlanHarness(workspaces ...string) *planHarness {
	h := newHarness()
	h.nar.risks = []string{riskText}
	h.nar.failure = failText
	return &planHarness{harness: h, pm: newPlanMCP(), workspaces: workspaces}
}

// addRun links a run of ws to the head SHA; details describe what HCP says.
func (h *planHarness) addRun(ws, runID, details string) {
	h.gl.statuses = append(h.gl.statuses, runStatus(ws, runID))
	h.pm.runs[runID] = details
}

// addFinished adds a finished run of ws whose plan JSON is planJSONFixture.
func (h *planHarness) addFinished(ws, runID, planID string) {
	h.addRun(ws, runID, runDetails(runID, "planned", planID, headSHA, ws, tfOrg))
	h.pm.planJSON[planID] = planJSONFixture
}

func (h *planHarness) addFailed(ws, runID, planID, log string) {
	h.addRun(ws, runID, runDetails(runID, "errored", planID, headSHA, ws, tfOrg))
	h.pm.logs[planID] = log
}

func (h *planHarness) planDeps() Deps {
	d := h.deps()
	d.MCP = h.pm
	d.Clock = &stepClock{now: time.Unix(1000, 0)}
	d.Config.TfeAddress = config.TfeAddress
	d.Config.TfeOrganization = tfOrg
	d.Config.Workspaces = h.workspaces
	d.Config.Waits = config.Waits{Initial: 30 * time.Second, Total: 5 * time.Minute}
	return d
}

func (h *planHarness) runPlan(t *testing.T) error {
	t.Helper()
	return RunPlan(context.Background(), h.planDeps(), input.Input{MRIID: 7, SHA: headSHA, PipelineID: 900})
}

func (h *planHarness) noteFor(t *testing.T, ws string) string {
	t.Helper()
	for _, n := range h.store.notes {
		if render.HasMarker(n.Body, "plan", ws) {
			return n.Body
		}
	}
	t.Fatalf("no plan note for %s among %d notes", ws, len(h.store.notes))
	return ""
}

func (h *planHarness) llmCalls() int {
	return len(h.nar.risksIn) + len(h.nar.blocksIn)
}

// --- tests ---

func TestRunPlan_NormalPathPostsSummaryAndRisks(t *testing.T) {
	h := newPlanHarness(wsProd)
	h.addFinished(wsProd, "run-OK1", "plan-1")
	if err := h.runPlan(t); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	body := h.store.body(t)
	mustContain(t, body,
		"<!-- tf-review:v1:plan:net-prod -->",
		"実行: "+runURLOf(wsProd, "run-OK1"),
		"追加 0 / 変更 1 / 削除 0 / 置換 1",
		"| `google_sql_user.app` | update | `password` |",
		"- "+riskText,
	)
	if len(h.nar.risksIn) != 1 || !reflect.DeepEqual(h.nar.risksIn[0], wantSummary) {
		t.Errorf("risks input = %+v, want %+v", h.nar.risksIn, wantSummary)
	}
	calls := h.pm.callsTo(plan.ToolGetPlanJSON)
	if len(calls) != 1 || calls[0].args["plan_id"] != "plan-1" || len(calls[0].args) != 1 {
		t.Errorf("plan JSON calls = %+v", calls)
	}
	for _, sha := range h.gl.statusSHAs {
		if sha != headSHA {
			t.Errorf("commit statuses read for %q, want head SHA", sha)
		}
	}
}

func TestRunPlan_NoPlanPostsNotice(t *testing.T) {
	h := newPlanHarness(wsProd)
	if err := h.runPlan(t); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	mustContain(t, h.store.body(t), "<!-- tf-review:v1:plan:net-prod -->", "このコミットに対応する plan が見つかりませんでした。")
	if len(h.pm.calls) != 0 || h.llmCalls() != 0 {
		t.Errorf("no MCP or LLM call expected: mcp=%v llm=%d", h.pm.calls, h.llmCalls())
	}
}

// aiLabelText is render's reference-only label for AI output (spec §6).
const aiLabelText = "_AIが作成した所見です。参考情報として扱ってください。_"

func TestRunPlan_FailedPlanAnalyzesErrorBlocks(t *testing.T) {
	h := newPlanHarness(wsProd)
	h.addFailed(wsProd, "run-ERR1", "plan-9", failedLogFixture)
	if err := h.runPlan(t); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	mustContain(t, h.store.body(t), "### plan 失敗", failText, "実行: "+runURLOf(wsProd, "run-ERR1"), aiLabelText)
	if len(h.nar.blocksIn) != 1 || !reflect.DeepEqual(h.nar.blocksIn[0], []string{errorBlock}) {
		t.Errorf("failure input = %q", h.nar.blocksIn)
	}
	calls := h.pm.callsTo(plan.ToolGetPlanLogs)
	if len(calls) != 1 || calls[0].args["plan_id"] != "plan-9" {
		t.Errorf("plan log calls = %+v", calls)
	}
	if len(h.nar.risksIn) != 0 || len(h.pm.callsTo(plan.ToolGetPlanJSON)) != 0 {
		t.Errorf("failed plan must not aggregate or ask for risks")
	}
}

func TestRunPlan_FailedPlanWithoutErrorBlocksSkipsLLM(t *testing.T) {
	h := newPlanHarness(wsProd)
	h.addFailed(wsProd, "run-ERR2", "plan-8", noBlockLogFixture)
	if err := h.runPlan(t); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	mustContain(t, h.store.body(t), "### plan 失敗", noErrorBlocksNotice)
	if strings.Contains(h.store.body(t), aiLabelText) {
		t.Errorf("fixed notice labeled as AI output")
	}
	if h.llmCalls() != 0 {
		t.Errorf("LLM called %d times, want 0", h.llmCalls())
	}
}

// A run of another commit may be a forged status, so the stale note of an
// earlier analysis is replaced by the unverified notice and an error returned.
func TestRunPlan_RunForOtherCommitPostsUnverifiedAndFails(t *testing.T) {
	h := newPlanHarness(wsProd)
	h.store.notes = []gitlab.Note{{ID: 50, AuthorID: botID, Body: render.Marker("plan", wsProd) + "\nold analysis"}}
	h.addRun(wsProd, "run-OLD1", runDetails("run-OLD1", "planned", "plan-1", otherSHA, wsProd, tfOrg))
	err := h.runPlan(t)
	if !errors.Is(err, plan.ErrSHAMismatch) {
		t.Fatalf("err = %v, want ErrSHAMismatch", err)
	}
	body := h.store.body(t)
	if !render.HasMarker(body, "plan", wsProd) || h.store.notes[0].ID != 50 {
		t.Errorf("unverified notice must replace the workspace note:\n%s", body)
	}
	mustContain(t, body, "このコミットの plan を確認できませんでした。")
	for _, s := range []string{"old analysis", "run-OLD1", "変更概要"} {
		if strings.Contains(body, s) {
			t.Errorf("unverified body contains %q", s)
		}
	}
	if h.llmCalls() != 0 || len(h.pm.callsTo(plan.ToolGetPlanJSON)) != 0 {
		t.Errorf("unverified run must not be analyzed: llm=%d", h.llmCalls())
	}
}

const planErrorNotice = "> 分析失敗: このコミットの plan を分析できませんでした。"

// assertPlanError checks ws got the fixed analysis-failed note: no run link,
// no summary.
func assertPlanError(t *testing.T, h *planHarness, ws string) {
	t.Helper()
	body := h.noteFor(t, ws)
	mustContain(t, body, planErrorNotice)
	for _, s := range []string{"実行:", "変更概要", "app.terraform.io"} {
		if strings.Contains(body, s) {
			t.Errorf("analysis-failed note contains %q:\n%s", s, body)
		}
	}
}

func TestRunPlan_RunOfOtherWorkspacePostsAnalysisFailedAndFails(t *testing.T) {
	for name, details := range map[string]string{
		"workspace":               runDetails("run-X1", "planned", "plan-1", headSHA, "other-ws", tfOrg),
		"org":                     runDetails("run-X1", "planned", "plan-1", headSHA, wsProd, "evil"),
		"workspace and other sha": runDetails("run-X1", "planned", "plan-1", otherSHA, "other-ws", tfOrg),
	} {
		t.Run(name, func(t *testing.T) {
			h := newPlanHarness(wsProd)
			h.addRun(wsProd, "run-X1", details)
			err := h.runPlan(t)
			if !errors.Is(err, plan.ErrWorkspaceMismatch) {
				t.Fatalf("err = %v, want ErrWorkspaceMismatch", err)
			}
			assertPlanError(t, h, wsProd)
		})
	}
}

func TestRunPlan_TimeoutPostsLastPhase(t *testing.T) {
	for status, want := range map[string]string{"pending": "待機中 (queued)", "planning": "実行中 (running)"} {
		t.Run(status, func(t *testing.T) {
			h := newPlanHarness(wsProd)
			h.addRun(wsProd, "run-SLOW1", runDetails("run-SLOW1", status, "plan-1", headSHA, wsProd, tfOrg))
			if err := h.runPlan(t); err != nil {
				t.Fatalf("RunPlan: %v", err)
			}
			mustContain(t, h.store.body(t), "待機時間内に plan が完了しませんでした。", want, "実行: "+runURLOf(wsProd, "run-SLOW1"))
			if strings.Contains(h.store.body(t), status) {
				t.Errorf("raw HCP status leaked into body")
			}
			if h.llmCalls() != 0 {
				t.Errorf("LLM called on timeout")
			}
		})
	}
}

func TestRunPlan_TwoWorkspacesGetSeparateNotesInSortedOrder(t *testing.T) {
	h := newPlanHarness(wsProd, wsDev)
	h.addFinished(wsProd, "run-P1", "plan-p")
	h.addFinished(wsDev, "run-D1", "plan-d")
	if err := h.runPlan(t); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	if len(h.store.notes) != 2 {
		t.Fatalf("notes = %d, want 2", len(h.store.notes))
	}
	if !render.HasMarker(h.store.notes[0].Body, "plan", wsDev) {
		t.Errorf("first note should be %s (sorted order):\n%s", wsDev, h.store.notes[0].Body)
	}
	mustContain(t, h.noteFor(t, wsDev), "workspace: `net-dev`", runURLOf(wsDev, "run-D1"))
	mustContain(t, h.noteFor(t, wsProd), "workspace: `net-prod`", runURLOf(wsProd, "run-P1"))
	if render.HasMarker(h.noteFor(t, wsDev), "plan", wsProd) {
		t.Errorf("dev note carries the prod marker")
	}
}

func TestRunPlan_OneWorkspaceFailingDoesNotStopOthers(t *testing.T) {
	h := newPlanHarness(wsProd, wsDev)
	h.addRun(wsDev, "run-X1", runDetails("run-X1", "planned", "plan-x", headSHA, "other-ws", tfOrg))
	h.addFinished(wsProd, "run-P1", "plan-p")
	err := h.runPlan(t)
	if !errors.Is(err, plan.ErrWorkspaceMismatch) {
		t.Fatalf("err = %v", err)
	}
	if len(h.store.notes) != 2 {
		t.Fatalf("notes = %d, want 2", len(h.store.notes))
	}
	assertPlanError(t, h, wsDev)
	mustContain(t, h.noteFor(t, wsProd), "- "+riskText)
}

func TestRunPlan_RiskLLMErrorPostsAnalysisFailed(t *testing.T) {
	llmErr := errors.New("model unavailable")
	h := newPlanHarness(wsProd)
	h.nar.risksErr = llmErr
	h.addFinished(wsProd, "run-OK1", "plan-1")
	if err := h.runPlan(t); !errors.Is(err, llmErr) {
		t.Fatalf("err = %v, want wrapped LLM error", err)
	}
	mustContain(t, h.store.body(t), "> 分析失敗: plan の概要のみ表示します。", "追加 0 / 変更 1 / 削除 0 / 置換 1")
}

// A failed plan must never look like a no-change plan, even without analysis.
func TestRunPlan_FailureLLMErrorPostsFailedNotice(t *testing.T) {
	llmErr := errors.New("model unavailable")
	h := newPlanHarness(wsProd)
	h.nar.failureErr = llmErr
	h.addFailed(wsProd, "run-ERR1", "plan-9", failedLogFixture)
	if err := h.runPlan(t); !errors.Is(err, llmErr) {
		t.Fatalf("err = %v, want wrapped LLM error", err)
	}
	body := h.store.body(t)
	mustContain(t, body, "### plan 失敗", failureAnalysisFailedNotice)
	for _, s := range []string{"追加 0", "分析失敗: plan の概要のみ", aiLabelText} {
		if strings.Contains(body, s) {
			t.Errorf("failed plan body contains %q:\n%s", s, body)
		}
	}
}

func TestRunPlan_StaleHeadIsNotAnError(t *testing.T) {
	h := newPlanHarness(wsProd)
	h.addFinished(wsProd, "run-OK1", "plan-1")
	h.store.sha = otherSHA
	if err := h.runPlan(t); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	if len(h.store.notes) != 0 {
		t.Errorf("stale head must not post, notes = %d", len(h.store.notes))
	}
	mustContain(t, h.logs.String(), "level=INFO msg=\"review: merge request head moved; skipping plan publish\"")
}

func TestRunPlan_StageErrorsPostAnalysisFailed(t *testing.T) {
	stageErr := errors.New("stage down")
	for name, setup := range map[string]func(h *planHarness){
		"plan JSON call": func(h *planHarness) {
			h.addFinished(wsProd, "run-OK1", "plan-1")
			h.pm.errs[plan.ToolGetPlanJSON] = stageErr
		},
		"run details call": func(h *planHarness) {
			h.addFinished(wsProd, "run-OK1", "plan-1")
			h.pm.errs[plan.ToolGetRunDetails] = stageErr
		},
		"plan logs call": func(h *planHarness) {
			h.addFailed(wsProd, "run-ERR1", "plan-9", failedLogFixture)
			h.pm.errs[plan.ToolGetPlanLogs] = stageErr
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newPlanHarness(wsProd)
			setup(h)
			if err := h.runPlan(t); !errors.Is(err, stageErr) {
				t.Fatalf("err = %v, want wrapping %v", err, stageErr)
			}
			assertPlanError(t, h, wsProd)
			if strings.Contains(h.noteFor(t, wsProd), stageErr.Error()) {
				t.Error("note carries error text")
			}
		})
	}
}

func TestRunPlan_WrongShapePlanJSONPostsAnalysisFailed(t *testing.T) {
	h := newPlanHarness(wsProd)
	h.addFinished(wsProd, "run-OK1", "plan-1")
	h.pm.planJSON["plan-1"] = `{"data":{"type":"plans"}}`
	if err := h.runPlan(t); err == nil {
		t.Fatal("want error")
	}
	assertPlanError(t, h, wsProd)
	if h.llmCalls() != 0 {
		t.Error("LLM called for an unreadable plan")
	}
}

func TestRunPlan_MissingPlanIDPostsAnalysisFailed(t *testing.T) {
	h := newPlanHarness(wsProd)
	h.addRun(wsProd, "run-OK1", runDetails("run-OK1", "planned", "", headSHA, wsProd, tfOrg))
	if err := h.runPlan(t); err == nil {
		t.Fatal("want error")
	}
	assertPlanError(t, h, wsProd)
}

func TestRunPlan_UnrecognizedHCPStatusPostsUnverified(t *testing.T) {
	h := newPlanHarness(wsProd)
	h.gl.statuses = []gitlab.CommitStatus{{
		Name: "HCP Terraform/" + tfOrg + "/" + wsProd, Status: "success",
		TargetURL: "https://app.terraform.io/app/" + tfOrg + "/" + wsProd + "/runs/run-NEW1",
	}}
	err := h.runPlan(t)
	if !errors.Is(err, plan.ErrUnrecognizedStatus) {
		t.Fatalf("err = %v, want ErrUnrecognizedStatus", err)
	}
	body := h.noteFor(t, wsProd)
	mustContain(t, body, "このコミットの plan を確認できませんでした。")
	if strings.Contains(body, "見つかりませんでした") || strings.Contains(body, "run-NEW1") {
		t.Errorf("misleading or leaking body:\n%s", body)
	}
	if strings.Contains(h.logs.String(), "run-NEW1") {
		t.Errorf("log leaks the status URL:\n%s", h.logs.String())
	}
}

// The plan wait ends postWaitReserve before the process deadline, even when
// waits.total would allow more, so the timeout notice still gets posted.
func TestRunPlan_WaitIsBoundedByDeadline(t *testing.T) {
	h := newPlanHarness(wsProd)
	h.addRun(wsProd, "run-SLOW1", runDetails("run-SLOW1", "planning", "plan-1", headSHA, wsProd, tfOrg))
	d := h.planDeps()
	clock := d.Clock.(*stepClock)
	start := clock.now
	d.Config.Waits = config.Waits{Initial: time.Minute, Total: 30 * time.Minute}
	ctx, cancel := context.WithTimeout(context.Background(), postWaitReserve+2*time.Minute)
	defer cancel()
	if err := RunPlan(ctx, d, input.Input{MRIID: 7, SHA: headSHA, PipelineID: 900}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	if waited := clock.now.Sub(start); waited > 2*time.Minute {
		t.Fatalf("waited %v, want at most the 2m left before the reserve", waited)
	}
	mustContain(t, h.store.body(t), "待機時間内に plan が完了しませんでした。")
}

func TestRunPlan_NoTimeLeftPostsAnalysisFailed(t *testing.T) {
	h := newPlanHarness(wsProd)
	h.addFinished(wsProd, "run-OK1", "plan-1")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := RunPlan(ctx, h.planDeps(), input.Input{MRIID: 7, SHA: headSHA, PipelineID: 900}); err == nil {
		t.Fatal("want error")
	}
	assertPlanError(t, h, wsProd)
	if len(h.pm.calls) != 0 {
		t.Errorf("MCP called without time to wait: %v", h.pm.calls)
	}
}

// Notices are posted on a detached context, so a run cancelled by the
// deadline or a signal still replaces the previous commit's note.
func TestRunPlan_CancelledRunStillPostsNotice(t *testing.T) {
	h := newPlanHarness(wsProd)
	h.addFinished(wsProd, "run-OK1", "plan-1")
	ctx, cancel := context.WithCancel(context.Background())
	h.nar.risksHook = cancel
	h.nar.risksErr = context.Canceled
	if err := RunPlan(ctx, h.planDeps(), input.Input{MRIID: 7, SHA: headSHA, PipelineID: 900}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	mustContain(t, h.store.body(t), "> 分析失敗: plan の概要のみ表示します。")
}

// Review Focus 1: plan values, tokens and raw MCP responses never reach the
// logs, the LLM input or any posted body.
func TestRunPlan_NoSecretsInLogsLLMInputOrBodies(t *testing.T) {
	h := newPlanHarness(wsProd, wsDev)
	h.addFinished(wsProd, "run-P1", "plan-p")
	h.addFailed(wsDev, "run-D1", "plan-d", failedLogFixture)
	if err := h.runPlan(t); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	if len(h.store.notes) != 2 || len(h.nar.risksIn) != 1 || len(h.nar.blocksIn) != 1 {
		t.Fatalf("flow incomplete: notes=%d risks=%d blocks=%d", len(h.store.notes), len(h.nar.risksIn), len(h.nar.blocksIn))
	}
	surfaces := map[string]string{
		"logs":       h.logs.String(),
		"risksInput": fmt.Sprintf("%+v", h.nar.risksIn),
		"failInput":  fmt.Sprintf("%+v", h.nar.blocksIn),
	}
	for i, n := range h.store.notes {
		surfaces[fmt.Sprintf("body%d", i)] = n.Body
	}
	forbidden := append([]string{tokenSecret, runSecret, "TFE_TOKEN", planJSONFixture, failedLogFixture,
		h.pm.runs["run-P1"], h.pm.runs["run-D1"]}, planSecrets...)
	assertNoForbidden(t, surfaces, forbidden)
}

// Error paths: a malformed plan JSON carrying a secret and an LLM failure on
// a failed plan must leave no secret in the returned error or the logs.
func TestRunPlan_ErrorPathsLeakNoSecrets(t *testing.T) {
	h := newPlanHarness(wsProd, wsDev)
	h.nar.failureErr = errors.New("model unavailable")
	brokenJSON := `{"variables":{"db_password":{"value":"VAR_SECRET_1a"}}, not json ` + tokenSecret
	h.addRun(wsProd, "run-P1", runDetails("run-P1", "planned", "plan-p", headSHA, wsProd, tfOrg))
	h.pm.planJSON["plan-p"] = brokenJSON
	h.addFailed(wsDev, "run-D1", "plan-d", failedLogFixture)
	err := h.runPlan(t)
	if err == nil {
		t.Fatal("want joined error from both workspaces")
	}
	surfaces := map[string]string{"err": err.Error(), "logs": h.logs.String()}
	forbidden := append([]string{tokenSecret, runSecret, "TFE_TOKEN", brokenJSON, failedLogFixture,
		h.pm.runs["run-P1"], h.pm.runs["run-D1"]}, planSecrets...)
	assertNoForbidden(t, surfaces, forbidden)
}

func assertNoForbidden(t *testing.T, surfaces map[string]string, forbidden []string) {
	t.Helper()
	for name, text := range surfaces {
		for _, s := range forbidden {
			if strings.Contains(text, s) {
				t.Errorf("%s contains forbidden text %.40q", name, s)
			}
		}
	}
}

func TestRunPlan_LLMTextIsCappedAtLLMOutputBytes(t *testing.T) {
	long := strings.Repeat("나", 3000)
	h := newPlanHarness(wsProd, wsDev)
	h.nar.risks = []string{long}
	h.nar.failure = long
	h.addFinished(wsProd, "run-P1", "plan-p")
	h.addFailed(wsDev, "run-D1", "plan-d", failedLogFixture)
	if err := h.runPlan(t); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	for _, ws := range []string{wsProd, wsDev} {
		line := lineWith(t, h.noteFor(t, ws), "나나나")
		if strings.Count(line, "나")*3 > 4000 || !strings.Contains(line, "…") {
			t.Errorf("%s: LLM text not capped at 4000 bytes: %d runes", ws, strings.Count(line, "나"))
		}
	}
}

// The fixed plan notices are posted to GitLab, so they are Japanese.
func TestPlanNoticesHaveNoHangul(t *testing.T) {
	for _, s := range []string{noErrorBlocksNotice, failureAnalysisFailedNotice} {
		for _, r := range s {
			if unicode.Is(unicode.Hangul, r) {
				t.Fatalf("Hangul in notice %q", s)
			}
		}
	}
}
