package review

import (
	"context"
	"errors"
	"fmt"

	"github.com/terraform-agent/internal/input"
)

// diagnosticTools are the allowlist logical names (phase0 A1) called only to
// log whether the TFE token works. Their results never fail the run.
var diagnosticTools = []string{"whoami", "getTokenPermissions"}

// errStale signals that the MR head moved on and the run should end quietly.
var errStale = errors.New("review: MR head moved on")

// Preflight checks the job inputs against GitLab only (d.GitLab, d.Config,
// d.Log), so callers can run it before starting the MCP server or the LLM.
// It returns false with a nil error when a newer push made the job stale, and
// an error when the MR is not from the allowed project, is a fork, or the
// pipeline is not the MR's pipeline for the SHA.
func Preflight(ctx context.Context, d Deps, in input.Input) (bool, error) {
	if err := checkMergeRequest(ctx, d, in); err != nil {
		if errors.Is(err, errStale) {
			d.Log.Info("review: newer push on the MR; skipping", "mr_iid", in.MRIID)
			return false, nil
		}
		return false, err
	}
	if err := checkPipeline(ctx, d, in); err != nil {
		return false, err
	}
	return true, nil
}

// Run runs Preflight, verifies the MCP tools, then runs the code review and
// the plan analysis. Both flows always run; their errors are joined. Run
// repeats Preflight even when the caller already ran it, so it is safe on its
// own and re-checks the MR head right before any work is published.
func Run(ctx context.Context, d Deps, in input.Input) error {
	proceed, err := Preflight(ctx, d, in)
	if err != nil || !proceed {
		return err
	}
	if err := d.Tools.VerifyTools(ctx); err != nil {
		return fmt.Errorf("review: verify MCP tools: %w", err)
	}
	runDiagnostics(ctx, d)
	codeErr := RunCode(ctx, d, in)
	planErr := RunPlan(ctx, d, in)
	return errors.Join(codeErr, planErr)
}

// checkMergeRequest fails unless the MR lives in, and comes from, the allowed
// project (fork MRs are out of scope). Only then is a moved head reported as
// errStale, so a disallowed MR can never be skipped silently.
func checkMergeRequest(ctx context.Context, d Deps, in input.Input) error {
	mr, err := d.GitLab.MergeRequest(ctx, in.MRIID)
	if err != nil {
		return fmt.Errorf("review: get merge request: %w", err)
	}
	allowed := d.Config.AllowedGitlabProjectID
	if mr.TargetProjectID != allowed || mr.SourceProjectID != allowed {
		return fmt.Errorf("review: MR !%d is not from the allowed project %d (target %d, source %d)",
			in.MRIID, allowed, mr.TargetProjectID, mr.SourceProjectID)
	}
	if mr.SHA != in.SHA {
		return errStale
	}
	return nil
}

// checkPipeline fails unless in.PipelineID is one of the MR's pipelines and
// ran for in.SHA.
func checkPipeline(ctx context.Context, d Deps, in input.Input) error {
	pipelines, err := d.GitLab.MergeRequestPipelines(ctx, in.MRIID)
	if err != nil {
		return fmt.Errorf("review: list MR pipelines: %w", err)
	}
	for _, p := range pipelines {
		if p.ID == int64(in.PipelineID) && p.SHA == in.SHA {
			return nil
		}
	}
	return fmt.Errorf("review: pipeline %d is not a pipeline of MR !%d for the given SHA", in.PipelineID, in.MRIID)
}

// runDiagnostics logs only each tool's name and outcome, never its payload or
// error text, and never fails.
func runDiagnostics(ctx context.Context, d Deps) {
	for _, name := range diagnosticTools {
		_, err := d.MCP.Call(ctx, name, map[string]any{})
		d.Log.Info("review: MCP diagnostic", "tool", name, "ok", err == nil)
	}
}
