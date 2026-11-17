package plan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/terraform-agent/internal/config"
	"github.com/terraform-agent/internal/gitlab"
	"github.com/terraform-agent/internal/tfmcp"
)

// Logical MCP tool names (allowlist keys, see docs/superpowers/notes/phase0.md A1).
const (
	ToolGetRunDetails    = "getRunDetails"
	ToolGetPlanJSON      = "getPlanJSONOutput"
	ToolGetPlanLogs      = "getPlanLogs"
	pollInterval         = 10 * time.Second
	runDetailsRunIDParam = "run_id"
)

var (
	// ErrNoPlan means no trusted HCP status appeared within the initial wait.
	ErrNoPlan = errors.New("plan: no HCP Terraform run found for commit")
	// ErrTimeout means the run did not reach a terminal status in time.
	ErrTimeout = errors.New("plan: timed out waiting for run")
)

// Phase is a coarse view of an HCP run status.
type Phase string

const (
	PhaseQueued   Phase = "queued"
	PhaseRunning  Phase = "running"
	PhaseFinished Phase = "finished"
	PhaseFailed   Phase = "failed"
)

// RunState is the last observed state of a run.
type RunState struct {
	Ref    RunRef
	Status string
	PlanID string
	Phase  Phase
}

// StatusSource lists commit statuses of the head SHA the caller bound it to.
type StatusSource interface {
	CommitStatuses(ctx context.Context) ([]gitlab.CommitStatus, error)
}

// Clock abstracts time so waiting can be tested.
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

// RealClock is the wall-clock Clock.
type RealClock struct{}

// Now returns the current time.
func (RealClock) Now() time.Time { return time.Now() }

// Sleep waits d or until ctx is done.
func (RealClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Wait finds the run (phase 1, bounded by w.Initial) and then polls it until
// it is finished or failed (phase 2, bounded by w.Total from the start).
func Wait(ctx context.Context, ss StatusSource, c tfmcp.Caller, src Source, w config.Waits, clock Clock) (RunState, error) {
	start := clock.Now()
	ref, err := findRunWithin(ctx, ss, src, w.Initial, start, clock)
	if err != nil {
		return RunState{}, err
	}
	for {
		st, err := runState(ctx, c, ref)
		if err != nil {
			return RunState{}, err
		}
		if st.Phase == PhaseFinished || st.Phase == PhaseFailed {
			return st, nil
		}
		if clock.Now().Sub(start) >= w.Total {
			return RunState{}, ErrTimeout
		}
		if err := clock.Sleep(ctx, pollInterval); err != nil {
			return RunState{}, err
		}
	}
}

func findRunWithin(ctx context.Context, ss StatusSource, src Source, initial time.Duration, start time.Time, clock Clock) (RunRef, error) {
	for {
		statuses, err := ss.CommitStatuses(ctx)
		if err != nil {
			return RunRef{}, fmt.Errorf("plan: list commit statuses: %w", err)
		}
		if ref, ok := FindRun(statuses, src); ok {
			return ref, nil
		}
		if clock.Now().Sub(start) >= initial {
			return RunRef{}, ErrNoPlan
		}
		if err := clock.Sleep(ctx, pollInterval); err != nil {
			return RunRef{}, err
		}
	}
}

func runState(ctx context.Context, c tfmcp.Caller, ref RunRef) (RunState, error) {
	out, err := c.Call(ctx, ToolGetRunDetails, map[string]any{runDetailsRunIDParam: ref.RunID})
	if err != nil {
		return RunState{}, fmt.Errorf("plan: get run details: %w", err)
	}
	d, err := parseRunDetails(out)
	if err != nil {
		return RunState{}, err
	}
	if d.status == "" {
		return RunState{}, errors.New("plan: run details have no status")
	}
	return RunState{Ref: ref, Status: d.status, PlanID: d.planID, Phase: phaseOf(d.status)}, nil
}

// phaseOf maps an HCP run status to a Phase. An unknown status counts as
// running, so the wait continues until Total expires.
func phaseOf(status string) Phase {
	switch status {
	case "pending", "plan_queued", "queuing":
		return PhaseQueued
	case "planned", "planned_and_finished", "planned_and_saved", "cost_estimated",
		"policy_checked", "policy_override", "policy_soft_failed", "post_plan_completed",
		"confirmed", "apply_queued", "applying", "applied":
		return PhaseFinished
	case "errored", "canceled", "discarded", "force_canceled":
		return PhaseFailed
	}
	return PhaseRunning
}

type runInfo struct {
	status string
	planID string
	sha    string
}

// parseRunDetails reads a run payload.
// PROVISIONAL (Phase 0 B): assumes JSON:API with data.attributes.status,
// data.relationships.plan.data.id and the commit SHA in data.attributes
// ["commit-sha"] or in an included ingress-attributes/configuration-versions
// entry. Errors never include the payload.
func parseRunDetails(payload string) (runInfo, error) {
	var doc struct {
		Data struct {
			Attributes    map[string]any `json:"attributes"`
			Relationships struct {
				Plan struct {
					Data struct {
						ID string `json:"id"`
					} `json:"data"`
				} `json:"plan"`
			} `json:"relationships"`
		} `json:"data"`
		Included []struct {
			Type       string         `json:"type"`
			Attributes map[string]any `json:"attributes"`
		} `json:"included"`
	}
	if err := json.Unmarshal([]byte(payload), &doc); err != nil {
		return runInfo{}, errors.New("plan: run details are not valid JSON")
	}
	d := runInfo{planID: doc.Data.Relationships.Plan.Data.ID}
	d.status, _ = doc.Data.Attributes["status"].(string)
	d.sha, _ = doc.Data.Attributes["commit-sha"].(string)
	for _, inc := range doc.Included {
		if d.sha != "" {
			break
		}
		if inc.Type == "ingress-attributes" || inc.Type == "configuration-versions" {
			d.sha, _ = inc.Attributes["commit-sha"].(string)
		}
	}
	return d, nil
}

// VerifyRunSHA fails closed: it errors when the run's commit SHA is missing
// or differs from wantSHA.
func VerifyRunSHA(runDetails string, wantSHA string) error {
	if wantSHA == "" {
		return errors.New("plan: expected commit SHA is empty")
	}
	d, err := parseRunDetails(runDetails)
	if err != nil {
		return err
	}
	if d.sha == "" {
		return errors.New("plan: run commit SHA not found")
	}
	if d.sha != wantSHA {
		return errors.New("plan: run commit SHA does not match MR head")
	}
	return nil
}
