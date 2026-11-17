// Command tf-review is the Cloud Run job entrypoint. It validates the job
// inputs, assembles the review dependencies and runs review.Run under a
// process deadline. All assembly lives in run so it can be tested.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/terraform-agent/internal/config"
	"github.com/terraform-agent/internal/gitlab"
	"github.com/terraform-agent/internal/httpx"
	"github.com/terraform-agent/internal/input"
	"github.com/terraform-agent/internal/llm"
	"github.com/terraform-agent/internal/logx"
	"github.com/terraform-agent/internal/plan"
	"github.com/terraform-agent/internal/review"
	"github.com/terraform-agent/internal/tfdocs"
	"github.com/terraform-agent/internal/tfmcp"
)

const (
	configPath    = "/app/config.json"
	gitlabTimeout = 30 * time.Second
	// llmTimeout bounds one Vertex AI request; a response is capped at a few
	// thousand output tokens, so minutes are ample.
	llmTimeout = 3 * time.Minute
)

// main is the only place that touches os.Getenv, and only to hand it to
// input.Parse via run. run logs its own error with the masking logger.
func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv); err != nil {
		os.Exit(1)
	}
}

// job is the validated startup state.
type job struct {
	in      input.Input
	cfg     config.Config
	secrets []string
}

// run returns its error after logging it. Errors before the masking logger
// exists come from input, config and token-file reads, which hold no secrets.
func run(ctx context.Context, args []string, getenv func(string) string) error {
	httpx.DisableEnvProxy()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, config.ProcessDeadline)
	defer cancel()
	j, err := setup(args, getenv)
	if err != nil {
		logx.New(os.Stderr, nil).Error("tf-review: startup failed", "error", err)
		return err
	}
	log := logx.New(os.Stderr, j.secrets)
	if err := execute(ctx, j, log); err != nil {
		log.Error("tf-review: run failed", "mr_iid", j.in.MRIID, "error", err)
		return err
	}
	log.Info("tf-review: done", "mr_iid", j.in.MRIID)
	return nil
}

// setup reads the tokens only to register them with the logger; the GitLab
// and MCP clients read the same files themselves with the same trimming.
func setup(args []string, getenv func(string) string) (job, error) {
	in, err := input.Parse(args, getenv)
	if err != nil {
		return job{}, err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return job{}, err
	}
	gitlabToken, err := readToken(cfg.TokenFiles.GitLab)
	if err != nil {
		return job{}, err
	}
	tfeToken, err := readToken(cfg.TokenFiles.TFE)
	if err != nil {
		return job{}, err
	}
	return job{in: in, cfg: cfg, secrets: []string{gitlabToken, tfeToken}}, nil
}

// readToken trims exactly like gitlab.NewClient and tfmcp.Start.
func readToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("tf-review: read token file: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// execute runs review.Preflight (GitLab only) before starting the MCP child
// process that holds the TFE token or creating ADC credentials, so a
// disallowed, fork or stale job never spawns them.
func execute(ctx context.Context, j job, log *slog.Logger) error {
	cfg := j.cfg
	gl, err := gitlab.NewClient(cfg.GitlabBaseURL, cfg.TokenFiles.GitLab, cfg.AllowedGitlabProjectID, httpx.NewClient(gitlabTimeout))
	if err != nil {
		return err
	}
	pre := review.Deps{GitLab: gl, Config: cfg, Log: log}
	if proceed, err := review.Preflight(ctx, pre, j.in); err != nil || !proceed {
		return err
	}
	mcp, err := tfmcp.Start(ctx, cfg, cfg.TokenFiles.TFE)
	if err != nil {
		return err
	}
	defer closeMCP(mcp, log)
	narrator, err := llm.NewVertexNarrator(ctx, cfg, httpx.NewClient(llmTimeout))
	if err != nil {
		return err
	}
	d := review.Deps{
		GitLab: gl, Notes: gl, MCP: mcp, Tools: mcp, Narrator: narrator,
		Docs: tfdocs.NewLookuper(), Config: cfg, Clock: plan.RealClock{}, Log: log,
	}
	return review.Run(ctx, d, j.in)
}

// closeMCP logs instead of failing: a finished review should not fail the job
// because the temp HOME could not be removed.
func closeMCP(c *tfmcp.Client, log *slog.Logger) {
	if err := c.Close(); err != nil {
		log.Warn("tf-review: close MCP client", "error", err)
	}
}
