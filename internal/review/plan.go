package review

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"

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
// does not stop the others; their errors are joined. Raw plan JSON, logs and
// MCP responses are never logged, returned in errors or posted.
func RunPlan(ctx context.Context, d Deps, in input.Input) error {
	u, err := url.Parse(d.Config.TfeAddress)
	if err != nil || u.Host == "" {
		return errors.New("review: tfeAddress has no host")
	}
	workspaces := slices.Clone(d.Config.Workspaces)
	slices.Sort(workspaces)
	var errs []error
	for _, ws := range workspaces {
		if err := planWorkspace(ctx, d, in, u.Host, ws); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func planWorkspace(ctx context.Context, d Deps, in input.Input, host, ws string) error {
	src := plan.Source{Host: host, Org: d.Config.TfeOrganization, Workspaces: []string{ws}, WantSHA: in.SHA}
	st, err := plan.Wait(ctx, statusSource{gl: d.GitLab, sha: in.SHA}, d.MCP, src, d.Config.Waits, d.Clock)
	if err != nil {
		return handleWaitError(ctx, d, in, ws, st, err)
	}
	d.Log.Info("review: plan run settled", "workspace", ws, "run", st.Ref.RunID, "phase", string(st.Phase))
	if st.PlanID == "" {
		return fmt.Errorf("review: plan run of workspace %s has no plan ID", ws)
	}
	view := render.PlanView{Workspace: ws, RunURL: runURL(d.Config.TfeOrganization, ws, st.Ref.RunID), SHA: in.SHA}
	if st.Phase == plan.PhaseFailed {
		return analyzeFailure(ctx, d, in, st.PlanID, view)
	}
	return analyzeChanges(ctx, d, in, st.PlanID, view)
}

// handleWaitError posts the no-plan and timeout notices, posts the unverified
// notice for a run of another commit (it may be a forged status, so the stale
// note must not survive; ruling R24), and records every other error
// (including a workspace or org mismatch) without posting.
func handleWaitError(ctx context.Context, d Deps, in input.Input, ws string, st plan.RunState, err error) error {
	switch {
	case errors.Is(err, plan.ErrNoPlan):
		d.Log.Info("review: no plan run found for commit", "workspace", ws)
		return publishPlan(ctx, d, in, render.PlanView{Workspace: ws, SHA: in.SHA, Status: render.PlanNoPlan})
	case errors.Is(err, plan.ErrTimeout):
		d.Log.Info("review: plan run did not finish in time", "workspace", ws, "run", st.Ref.RunID, "phase", string(st.Phase))
		return publishPlan(ctx, d, in, render.PlanView{
			Workspace: ws, RunURL: runURL(d.Config.TfeOrganization, ws, st.Ref.RunID), SHA: in.SHA,
			Status: render.PlanTimeout, Queued: st.Phase == plan.PhaseQueued,
		})
	case errors.Is(err, plan.ErrSHAMismatch):
		d.Log.Warn("review: plan run is for another commit; posting unverified notice", "workspace", ws)
		return publishUnverified(ctx, d, in, ws, err)
	}
	return fmt.Errorf("review: wait for plan run of workspace %s: %w", ws, err)
}

// publishUnverified posts the unverified notice (no RunURL, no summary) and
// returns the binding error, joined with any publish error.
func publishUnverified(ctx context.Context, d Deps, in input.Input, ws string, bindErr error) error {
	wrapped := fmt.Errorf("review: plan run of workspace %s not bound to commit: %w", ws, bindErr)
	view := render.PlanView{Workspace: ws, SHA: in.SHA, Status: render.PlanUnverified}
	if err := publishPlan(ctx, d, in, view); err != nil {
		return errors.Join(wrapped, err)
	}
	return wrapped
}

// analyzeChanges aggregates the plan JSON into a value-free summary and asks
// the LLM for risks. PROVISIONAL (Phase 0 B): the getPlanJSONOutput response
// text is the plan JSON itself.
func analyzeChanges(ctx context.Context, d Deps, in input.Input, planID string, view render.PlanView) error {
	raw, err := d.MCP.Call(ctx, plan.ToolGetPlanJSON, map[string]any{planIDParam: planID})
	if err != nil {
		return fmt.Errorf("review: get plan JSON of workspace %s: %w", view.Workspace, err)
	}
	sum, err := plan.Aggregate([]byte(raw), maxTargets)
	if err != nil {
		return fmt.Errorf("review: aggregate plan of workspace %s: %w", view.Workspace, err)
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
		return fmt.Errorf("review: get plan logs of workspace %s: %w", view.Workspace, err)
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
	if err := publishPlan(ctx, d, in, view); err != nil {
		return errors.Join(llmErr, err)
	}
	return llmErr
}

// publishPlanAnalysisFailed posts the analysis-failed variant (finished
// plans only), then returns llmErr (joined with any publish error).
func publishPlanAnalysisFailed(ctx context.Context, d Deps, in input.Input, view render.PlanView, llmErr error) error {
	view.Status = render.PlanAnalysisFailed
	view.Risks = nil
	if err := publishPlan(ctx, d, in, view); err != nil {
		return errors.Join(llmErr, err)
	}
	return llmErr
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
