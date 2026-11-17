package plan

import (
	"context"
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
	// ErrSHAMismatch means the run was created from another commit than the
	// MR head (typically an older push).
	ErrSHAMismatch = errors.New("plan: run commit SHA does not match MR head")
	// ErrWorkspaceMismatch means the run belongs to another org or workspace
	// than the one it was linked as (a possible forgery).
	ErrWorkspaceMismatch = errors.New("plan: run does not belong to the configured org and workspace")
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
// Every poll verifies the run's workspace, org and commit SHA against src and
// fails closed, so no state is returned for a run that is not ours. On
// ErrTimeout the last observed state is returned so callers can tell queued
// from running.
func Wait(ctx context.Context, ss StatusSource, c tfmcp.Caller, src Source, w config.Waits, clock Clock) (RunState, error) {
	start := clock.Now()
	ref, err := findRunWithin(ctx, ss, src, w.Initial, start, clock)
	if err != nil {
		return RunState{}, err
	}
	for {
		st, err := runState(ctx, c, src, ref)
		if err != nil {
			return RunState{}, err
		}
		if st.Phase == PhaseFinished || st.Phase == PhaseFailed {
			return st, nil
		}
		if err := sleepWithin(ctx, clock, start, w.Total); err != nil {
			if errors.Is(err, ErrTimeout) {
				return st, err
			}
			return RunState{}, err
		}
	}
}

// sleepWithin sleeps min(pollInterval, remaining) and returns ErrTimeout when
// limit has already elapsed since start.
func sleepWithin(ctx context.Context, clock Clock, start time.Time, limit time.Duration) error {
	remaining := limit - clock.Now().Sub(start)
	if remaining <= 0 {
		return ErrTimeout
	}
	return clock.Sleep(ctx, min(pollInterval, remaining))
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
		if err := sleepWithin(ctx, clock, start, initial); err != nil {
			if errors.Is(err, ErrTimeout) {
				return RunRef{}, ErrNoPlan
			}
			return RunRef{}, err
		}
	}
}

func runState(ctx context.Context, c tfmcp.Caller, src Source, ref RunRef) (RunState, error) {
	out, err := c.Call(ctx, ToolGetRunDetails, map[string]any{runDetailsRunIDParam: ref.RunID})
	if err != nil {
		return RunState{}, fmt.Errorf("plan: get run details: %w", err)
	}
	d, err := parseRunDetails(out)
	if err != nil {
		return RunState{}, err
	}
	if err := d.verify(src, ref); err != nil {
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
