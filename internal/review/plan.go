package review

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/terraform-agent/internal/config"
	"github.com/terraform-agent/internal/gitlab"
	"github.com/terraform-agent/internal/input"
	"github.com/terraform-agent/internal/plan"
	"github.com/terraform-agent/internal/publish"
	"github.com/terraform-agent/internal/render"
)

const (
	// maxTargets caps the plan summary targets handed to the LLM and the body.
	// Aggregate sets Truncated when it cuts; limits.planSummaryBytes is not
	// enforced separately (controller ruling R23).
	maxTargets = 100
	// maxErrorBlockBytes caps the error blocks handed to the LLM. No
	// config.Limits field is meant for plan logs (diff, docs, plan summary and
	// LLM output are other data), so a fixed 8 KiB is used.
	maxErrorBlockBytes = 8 * 1024
	// planIDParam is the argument name of getPlanJSONOutput and getPlanLogs.
	// PROVISIONAL (Phase 0 B): the input schema needs a token to inspect.
	planIDParam = "plan_id"
	planKind    = "plan"
	// postWaitReserve is kept between the end of a plan wait and the process
	// deadline for the plan JSON or log call (MCP calls time out after 60s),
	// one LLM call (3m client timeout) and publishing.
	postWaitReserve = 5 * time.Minute
)

// noErrorBlocksNotice is posted for a failed plan whose log has no error block.
const noErrorBlocksNotice = "plan 로그에서 에러 블록을 찾지 못했습니다. HCP Terraform 실행 화면에서 원인을 확인하세요."

// failureAnalysisFailedNotice is posted for a failed plan when the LLM could
// not explain its error blocks (ruling R25: never the no-change summary).
const failureAnalysisFailedNotice = "plan이 실패했습니다. 에러 분석을 생성하지 못했습니다. HCP Terraform 실행 화면에서 확인하세요."

// statusSource binds commit status listing to the MR head SHA.
type statusSource struct {
	gl  GitLabReader
	sha string
}

func (s statusSource) CommitStatuses(ctx context.Context) ([]gitlab.CommitStatus, error) {
	return s.gl.CommitStatuses(ctx, s.sha)
}

// RunPlan analyzes the HCP Terraform plan of every configured workspace, in
// sorted order, and upserts one plan note per workspace. A failing workspace
// does not stop the others; their errors are joined. Any failing stage posts
// the analysis-failed note for its workspace (spec §5). Raw plan JSON, logs
// and MCP responses are never logged, returned in errors or posted.
func RunPlan(ctx context.Context, d Deps, in input.Input) error {
	u, err := url.Parse(d.Config.TfeAddress)
	if err != nil || u.Host == "" {
		return errors.New("review: tfeAddress has no host")
	}
	var errs []error
	for _, ws := range sortedWorkspaces(d) {
		if err := planWorkspace(ctx, d, in, u.Host, ws); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func planWorkspace(ctx context.Context, d Deps, in input.Input, host, ws string) error {
	waits, ok := waitBudget(ctx, d.Config.Waits)
	if !ok {
		return failPlan(ctx, d, in, ws, fmt.Errorf("review: no time left to wait for the plan of workspace %s", ws))
	}
	src := plan.Source{Host: host, Org: d.Config.TfeOrganization, Workspaces: []string{ws}, WantSHA: in.SHA}
	st, err := plan.Wait(ctx, statusSource{gl: d.GitLab, sha: in.SHA}, d.MCP, src, waits, d.Clock)
	if err != nil {
		return handleWaitError(ctx, d, in, ws, st, err)
	}
	d.Log.Info("review: plan run settled", "workspace", ws, "run", st.Ref.RunID, "phase", string(st.Phase))
	if st.PlanID == "" {
		return failPlan(ctx, d, in, ws, fmt.Errorf("review: plan run of workspace %s has no plan ID", ws))
	}
	view := render.PlanView{Workspace: ws, RunURL: runURL(d.Config.TfeOrganization, ws, st.Ref.RunID), SHA: in.SHA}
	if st.Phase == plan.PhaseFailed {
		return analyzeFailure(ctx, d, in, st.PlanID, view)
	}
	return analyzeChanges(ctx, d, in, st.PlanID, view)
}

// waitBudget bounds the plan wait by the process deadline: Total becomes at
// most the time left minus postWaitReserve, and Initial at most Total. It
// reports false when no time is left to wait at all.
func waitBudget(ctx context.Context, w config.Waits) (config.Waits, bool) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return w, true
	}
	left := time.Until(deadline) - postWaitReserve
	if left <= 0 {
		return config.Waits{}, false
	}
	total := min(w.Total, left)
	return config.Waits{Initial: min(w.Initial, total), Total: total}, true
}

// handleWaitError posts the no-plan and timeout notices, and the unverified
// notice for a run of another commit (it may be a forged status, so the stale
// note must not survive; ruling R24) or for HCP statuses that could not be
// recognized (ruling R31). Every other error, including a workspace or org
// mismatch, posts the analysis-failed note.
func handleWaitError(ctx context.Context, d Deps, in input.Input, ws string, st plan.RunState, err error) error {
	switch {
	case errors.Is(err, plan.ErrNoPlan):
		d.Log.Info("review: no plan run found for commit", "workspace", ws)
		return postPlan(ctx, d, in, render.PlanView{Workspace: ws, SHA: in.SHA, Status: render.PlanNoPlan})
	case errors.Is(err, plan.ErrTimeout):
		d.Log.Info("review: plan run did not finish in time", "workspace", ws, "run", st.Ref.RunID, "phase", string(st.Phase))
		return postPlan(ctx, d, in, render.PlanView{
			Workspace: ws, RunURL: runURL(d.Config.TfeOrganization, ws, st.Ref.RunID), SHA: in.SHA,
			Status: render.PlanTimeout, Queued: st.Phase == plan.PhaseQueued,
		})
	case errors.Is(err, plan.ErrSHAMismatch):
		d.Log.Warn("review: plan run is for another commit; posting unverified notice", "workspace", ws)
		return publishUnverified(ctx, d, in, ws, err)
	case errors.Is(err, plan.ErrUnrecognizedStatus):
		// The error text carries only the count of such statuses.
		d.Log.Warn("review: HCP status not recognized; posting unverified notice", "workspace", ws, "err", err)
		return publishUnverified(ctx, d, in, ws, err)
	}
	return failPlan(ctx, d, in, ws, fmt.Errorf("review: wait for plan run of workspace %s: %w", ws, err))
}

// postPlan posts a notice that is not an error (no plan, timeout) on the
// detached notice context: the wait may have used up most of the run's time.
func postPlan(ctx context.Context, d Deps, in input.Input, view render.PlanView) error {
	return withNotice(ctx, nil, func(nctx context.Context) error { return publishPlan(nctx, d, in, view) })
}

// publishUnverified posts the unverified notice (no RunURL, no summary) and
// returns the binding error, joined with any publish error.
func publishUnverified(ctx context.Context, d Deps, in input.Input, ws string, bindErr error) error {
	wrapped := fmt.Errorf("review: plan run of workspace %s not bound to commit: %w", ws, bindErr)
	view := render.PlanView{Workspace: ws, SHA: in.SHA, Status: render.PlanUnverified}
	return withNotice(ctx, wrapped, func(nctx context.Context) error { return publishPlan(nctx, d, in, view) })
}

// analyzeChanges aggregates the plan JSON into a value-free summary and asks
// the LLM for risks. PROVISIONAL (Phase 0 B): the getPlanJSONOutput response
// text is the plan JSON itself.
func analyzeChanges(ctx context.Context, d Deps, in input.Input, planID string, view render.PlanView) error {
	raw, err := d.MCP.Call(ctx, plan.ToolGetPlanJSON, map[string]any{planIDParam: planID})
	if err != nil {
		return failPlan(ctx, d, in, view.Workspace, fmt.Errorf("review: get plan JSON of workspace %s: %w", view.Workspace, err))
	}
	sum, err := plan.Aggregate([]byte(raw), maxTargets)
	if err != nil {
		return failPlan(ctx, d, in, view.Workspace, fmt.Errorf("review: aggregate plan of workspace %s: %w", view.Workspace, err))
	}
	view.Summary = sum
	risks, err := d.Narrator.PlanRisks(ctx, sum)
	if err != nil {
		return publishPlanAnalysisFailed(ctx, d, in, view, fmt.Errorf("review: plan risks of workspace %s: %w", view.Workspace, err))
	}
	view.Status = render.PlanOK
	view.Risks = risks
	return publishPlan(ctx, d, in, view)
}

// analyzeFailure hands only the log's error blocks to the LLM. Without any
// error block it posts a fixed notice and makes no LLM call.
func analyzeFailure(ctx context.Context, d Deps, in input.Input, planID string, view render.PlanView) error {
	log, err := d.MCP.Call(ctx, plan.ToolGetPlanLogs, map[string]any{planIDParam: planID})
	if err != nil {
		return failPlan(ctx, d, in, view.Workspace, fmt.Errorf("review: get plan logs of workspace %s: %w", view.Workspace, err))
	}
	blocks := plan.ExtractErrorBlocks(log, maxErrorBlockBytes)
	view.Status = render.PlanFailed
	if len(blocks) == 0 {
		d.Log.Info("review: failed plan has no error block", "workspace", view.Workspace)
		view.Failure = noErrorBlocksNotice
		return publishPlan(ctx, d, in, view)
	}
	text, err := d.Narrator.PlanFailure(ctx, blocks)
	if err != nil {
		return publishFailureUnexplained(ctx, d, in, view, fmt.Errorf("review: plan failure of workspace %s: %w", view.Workspace, err))
	}
	view.Failure = text
	return publishPlan(ctx, d, in, view)
}

// runURL builds the run link from trusted values only: the fixed HCP address,
// the configured org and workspace, and a run ID that plan validated.
func runURL(org, ws, runID string) string {
	return config.TfeAddress + "/app/" + url.PathEscape(org) + "/" + url.PathEscape(ws) + "/runs/" + runID
}

// publishFailureUnexplained keeps a failed plan rendered as failed, with the
// fixed notice instead of an LLM explanation, then returns llmErr (joined
// with any publish error).
func publishFailureUnexplained(ctx context.Context, d Deps, in input.Input, view render.PlanView, llmErr error) error {
	view.Failure = failureAnalysisFailedNotice
	return withNotice(ctx, llmErr, func(nctx context.Context) error { return publishPlan(nctx, d, in, view) })
}

// publishPlanAnalysisFailed posts the analysis-failed variant (finished
// plans only), then returns llmErr (joined with any publish error).
func publishPlanAnalysisFailed(ctx context.Context, d Deps, in input.Input, view render.PlanView, llmErr error) error {
	view.Status = render.PlanAnalysisFailed
	view.Risks = nil
	return withNotice(ctx, llmErr, func(nctx context.Context) error { return publishPlan(nctx, d, in, view) })
}

// publishPlan upserts the workspace's plan note. A stale head is not an
// error: a newer run will post for the new head.
func publishPlan(ctx context.Context, d Deps, in input.Input, view render.PlanView) error {
	target := publish.Target{Kind: planKind, Scope: view.Workspace}
	err := publish.Upsert(ctx, d.Notes, in.MRIID, in.SHA, target, render.Plan(view))
	if errors.Is(err, publish.ErrStale) {
		d.Log.Info("review: merge request head moved; skipping plan publish", "workspace", view.Workspace)
		return nil
	}
	if err != nil {
		return fmt.Errorf("review: publish plan of workspace %s: %w", view.Workspace, err)
	}
	return nil
}
