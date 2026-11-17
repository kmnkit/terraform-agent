// Package review orchestrates the code review and plan analysis flows.
package review

import (
	"context"
	"log/slog"

	"github.com/terraform-agent/internal/config"
	"github.com/terraform-agent/internal/gitlab"
	"github.com/terraform-agent/internal/llm"
	"github.com/terraform-agent/internal/model"
	"github.com/terraform-agent/internal/plan"
	"github.com/terraform-agent/internal/publish"
	"github.com/terraform-agent/internal/tfdocs"
	"github.com/terraform-agent/internal/tfmcp"
)

// GitLabReader is the read-only subset of the GitLab client the flows use.
type GitLabReader interface {
	MergeRequestDiffs(ctx context.Context, iid int) ([]gitlab.FileDiff, error)
	FileAtRef(ctx context.Context, path, ref string) ([]byte, error)
	PipelineJobs(ctx context.Context, pipelineID int64) ([]gitlab.Job, error)
	JobArtifact(ctx context.Context, jobID int64, artifactPath string) ([]byte, error)
	// MergeRequest, MergeRequestPipelines and CommitStatuses are not used by
	// the code flow; they are here for the plan flow and entrypoint (Tasks
	// 13/14) so Deps does not churn.
	MergeRequest(ctx context.Context, iid int) (gitlab.MergeRequest, error)
	MergeRequestPipelines(ctx context.Context, iid int) ([]gitlab.Pipeline, error)
	CommitStatuses(ctx context.Context, sha string) ([]gitlab.CommitStatus, error)
}

// DocLookuper fetches a resource's documentation excerpt.
type DocLookuper interface {
	Lookup(ctx context.Context, c tfmcp.Caller, p model.ProviderRef, resourceType string,
		attrs []string, maxBytes int) (tfdocs.Excerpt, error)
}

// ToolVerifier checks that every allowlisted MCP tool is served.
type ToolVerifier interface {
	VerifyTools(ctx context.Context) error
}

// Deps are the collaborators of the review flows.
type Deps struct {
	GitLab   GitLabReader
	Notes    publish.NoteStore
	MCP      tfmcp.Caller
	Tools    ToolVerifier // used by Run's startup checks
	Narrator llm.Narrator
	Docs     DocLookuper
	Config   config.Config
	Clock    plan.Clock // used by the plan flow (Task 13)
	Log      *slog.Logger
}

var (
	_ GitLabReader      = (*gitlab.Client)(nil)
	_ publish.NoteStore = (*gitlab.Client)(nil)
	_ DocLookuper       = (*tfdocs.Lookuper)(nil)
	_ ToolVerifier      = (*tfmcp.Client)(nil)
)
