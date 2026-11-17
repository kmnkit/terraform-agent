package review

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/terraform-agent/internal/input"
	"github.com/terraform-agent/internal/model"
	"github.com/terraform-agent/internal/render"
	"github.com/terraform-agent/internal/tfdocs"
)

// noticeTimeout bounds posting one notice on a detached context.
const noticeTimeout = 60 * time.Second

// withNotice posts a notice with post and returns cause joined with any
// publish error (cause may be nil). The notice context ignores the
// cancellation of ctx, so a run stopped by the process deadline or a signal
// still tries to replace the previous commit's note (spec §5), but it is
// bounded by noticeTimeout. Notices never carry error text.
func withNotice(ctx context.Context, cause error, post func(context.Context) error) error {
	nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), noticeTimeout)
	defer cancel()
	if err := post(nctx); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// failCode posts the code analysis-failed note (static checks when known, no
// findings) and returns cause joined with any publish error.
func failCode(ctx context.Context, d Deps, in input.Input, checks []model.CheckResult, cause error) error {
	view := render.CodeReviewView{SHA: in.SHA, Checks: checks, AnalysisFailed: true, VersionBasis: string(tfdocs.BasisNone)}
	return withNotice(ctx, cause, func(nctx context.Context) error { return publishBody(nctx, d, in, view) })
}

// failPlan posts the plan analysis-failed note of ws (no run link, no
// summary) and returns cause joined with any publish error.
func failPlan(ctx context.Context, d Deps, in input.Input, ws string, cause error) error {
	view := render.PlanView{Workspace: ws, SHA: in.SHA, Status: render.PlanError}
	return withNotice(ctx, cause, func(nctx context.Context) error { return publishPlan(nctx, d, in, view) })
}

// FailStartup posts the analysis-failed notes for a run that passed Preflight
// but could not start its flows (MCP tool check, MCP server or LLM client):
// the code note, with static checks when they can be read, and the plan note
// of every configured workspace. It needs only d.GitLab, d.Notes, d.Config
// and d.Log, and returns cause joined with any publish error.
func FailStartup(ctx context.Context, d Deps, in input.Input, cause error) error {
	errs := []error{cause, failCode(ctx, d, in, startupChecks(ctx, d, in), nil)}
	for _, ws := range sortedWorkspaces(d) {
		errs = append(errs, failPlan(ctx, d, in, ws, nil))
	}
	return errors.Join(errs...)
}

// startupChecks reads the static checks for a startup failure note; on error
// the note goes without them.
func startupChecks(ctx context.Context, d Deps, in input.Input) []model.CheckResult {
	nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), noticeTimeout)
	defer cancel()
	checks, err := staticChecks(nctx, d, in.PipelineID)
	if err != nil {
		d.Log.Warn("review: static checks unavailable for the failure note", "err", err)
		return nil
	}
	return checks
}

func sortedWorkspaces(d Deps) []string {
	ws := slices.Clone(d.Config.Workspaces)
	slices.Sort(ws)
	return ws
}
