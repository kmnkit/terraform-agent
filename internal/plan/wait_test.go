package plan

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/terraform-agent/internal/config"
	"github.com/terraform-agent/internal/gitlab"
)

type fakeClock struct {
	now    time.Time
	sleeps int
	onSlp  func(n int)
}

func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.now = c.now.Add(d)
	c.sleeps++
	if c.onSlp != nil {
		c.onSlp(c.sleeps)
	}
	return nil
}

type fakeStatuses struct {
	byCall func(n int) []gitlab.CommitStatus
	calls  int
	err    error
}

func (f *fakeStatuses) CommitStatuses(context.Context) ([]gitlab.CommitStatus, error) {
	f.calls++
	return f.byCall(f.calls), f.err
}

type fakeCaller struct {
	statuses []string
	n        int
	gotName  string
	gotArgs  map[string]any
	err      error
}

func (f *fakeCaller) Call(_ context.Context, name string, args map[string]any) (string, error) {
	f.gotName, f.gotArgs = name, args
	if f.err != nil {
		return "", f.err
	}
	i := f.n
	if i >= len(f.statuses) {
		i = len(f.statuses) - 1
	}
	f.n++
	return runJSON(f.statuses[i], "plan-xyz", "abc123"), nil
}

func runJSON(status, planID, sha string) string {
	return runJSONWS(status, planID, sha, "ws-1", "net-prod", `"relationships":{"organization":{"data":{"id":"acme"}}}`)
}

// runJSONWS builds a run payload. wsTail is the raw JSON members appended to
// the included workspaces entry (relationships and/or more attributes).
func runJSONWS(status, planID, sha, wsID, wsName, wsTail string) string {
	return `{"data":{"id":"run-GOOD1","attributes":{"status":"` + status + `","commit-sha":"` + sha + `"},` +
		`"relationships":{"plan":{"data":{"id":"` + planID + `","type":"plans"}},` +
		`"workspace":{"data":{"id":"` + wsID + `","type":"workspaces"}}}},` +
		`"included":[{"id":"ws-1","type":"workspaces","attributes":{"name":"` + wsName + `"},` + wsTail + `}]}`
}

// fixedCaller always returns payload.
type fixedCaller struct{ payload string }

func (f fixedCaller) Call(context.Context, string, map[string]any) (string, error) {
	return f.payload, nil
}

var goodStatus = []gitlab.CommitStatus{{Name: "Terraform Cloud/acme/net-prod", Status: "running", TargetURL: "https://app.terraform.io/app/acme/net-prod/runs/run-GOOD1"}}

var testWaits = config.Waits{Initial: 30 * time.Second, Total: 5 * time.Minute}

func newClock() *fakeClock { return &fakeClock{now: time.Unix(1000, 0)} }

func TestWaitNoPlan(t *testing.T) {
	ss := &fakeStatuses{byCall: func(int) []gitlab.CommitStatus { return nil }}
	clk := newClock()
	start := clk.now
	_, err := Wait(context.Background(), ss, &fakeCaller{}, testSource, testWaits, clk)
	if !errors.Is(err, ErrNoPlan) {
		t.Fatalf("err = %v", err)
	}
	if got := clk.now.Sub(start); got != testWaits.Initial {
		t.Fatalf("elapsed = %v, want %v", got, testWaits.Initial)
	}
}

func TestWaitTimeoutReturnsLastObservedPhase(t *testing.T) {
	for status, want := range map[string]Phase{"pending": PhaseQueued, "planning": PhaseRunning, "mystery": PhaseRunning} {
		ss := &fakeStatuses{byCall: func(int) []gitlab.CommitStatus { return goodStatus }}
		clk := newClock()
		start := clk.now
		st, err := Wait(context.Background(), ss, &fakeCaller{statuses: []string{status}}, testSource, testWaits, clk)
		if !errors.Is(err, ErrTimeout) {
			t.Fatalf("%s: err = %v", status, err)
		}
		if st.Phase != want || st.Status != status || st.Ref.RunID != "run-GOOD1" {
			t.Fatalf("%s: state = %+v", status, st)
		}
		if got := clk.now.Sub(start); got != testWaits.Total {
			t.Fatalf("%s: elapsed = %v, want %v", status, got, testWaits.Total)
		}
	}
}

func TestWaitTotalIsMeasuredFromStartAcrossPhases(t *testing.T) {
	ss := &fakeStatuses{byCall: func(n int) []gitlab.CommitStatus {
		if n < 3 {
			return nil
		}
		return goodStatus
	}}
	clk := newClock()
	start := clk.now
	_, err := Wait(context.Background(), ss, &fakeCaller{statuses: []string{"planning"}}, testSource, testWaits, clk)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v", err)
	}
	if got := clk.now.Sub(start); got != testWaits.Total {
		t.Fatalf("elapsed = %v, want %v (phase 1 time must count)", got, testWaits.Total)
	}
}

func TestWaitSleepClampedToRemaining(t *testing.T) {
	w := config.Waits{Initial: 15 * time.Second, Total: 25 * time.Second}
	ss := &fakeStatuses{byCall: func(int) []gitlab.CommitStatus { return goodStatus }}
	clk := newClock()
	start := clk.now
	_, err := Wait(context.Background(), ss, &fakeCaller{statuses: []string{"planning"}}, testSource, w, clk)
	if !errors.Is(err, ErrTimeout) || clk.now.Sub(start) != w.Total {
		t.Fatalf("err=%v elapsed=%v", err, clk.now.Sub(start))
	}
}

func TestWaitRejectsRunBoundToOtherIdentity(t *testing.T) {
	cases := map[string]string{
		"other workspace":   runJSONWS("planned", "p", "abc123", "ws-1", "other-ws", `"relationships":{"organization":{"data":{"id":"acme"}}}`),
		"other org":         runJSONWS("planned", "p", "abc123", "ws-1", "net-prod", `"relationships":{"organization":{"data":{"id":"evil"}}}`),
		"no org":            runJSONWS("planned", "p", "abc123", "ws-1", "net-prod", `"links":{}`),
		"no workspace rel":  `{"data":{"attributes":{"status":"planned","commit-sha":"abc123"}}}`,
		"workspace id gap":  runJSONWS("planned", "p", "abc123", "ws-999", "net-prod", `"relationships":{"organization":{"data":{"id":"acme"}}}`),
		"sha mismatch":      runJSON("planned", "p", "deadbeef"),
		"sha missing":       runJSON("planned", "p", ""),
		"queued but forged": runJSONWS("pending", "p", "abc123", "ws-1", "other-ws", `"relationships":{"organization":{"data":{"id":"acme"}}}`),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			ss := &fakeStatuses{byCall: func(int) []gitlab.CommitStatus { return goodStatus }}
			st, err := Wait(context.Background(), ss, fixedCaller{payload}, testSource, testWaits, newClock())
			if err == nil || errors.Is(err, ErrTimeout) || errors.Is(err, ErrNoPlan) {
				t.Fatalf("err = %v", err)
			}
			if st.Phase != "" || strings.Contains(err.Error(), "other-ws") {
				t.Fatalf("state=%+v err=%v", st, err)
			}
		})
	}
}

func TestWaitAcceptsOrgFromAttribute(t *testing.T) {
	payload := runJSONWS("planned", "p", "abc123", "ws-1", "net-prod", `"x":{}`)
	payload = strings.Replace(payload, `"attributes":{"name":"net-prod"}`, `"attributes":{"name":"net-prod","organization-name":"acme"}`, 1)
	ss := &fakeStatuses{byCall: func(int) []gitlab.CommitStatus { return goodStatus }}
	st, err := Wait(context.Background(), ss, fixedCaller{payload}, testSource, testWaits, newClock())
	if err != nil || st.Phase != PhaseFinished {
		t.Fatalf("state=%+v err=%v", st, err)
	}
}

func TestWaitRequiresWantSHA(t *testing.T) {
	src := testSource
	src.WantSHA = ""
	ss := &fakeStatuses{byCall: func(int) []gitlab.CommitStatus { return goodStatus }}
	_, err := Wait(context.Background(), ss, fixedCaller{runJSON("planned", "p", "abc123")}, src, testWaits, newClock())
	if err == nil || errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v", err)
	}
}

func TestWaitQueuedRunningFinished(t *testing.T) {
	ss := &fakeStatuses{byCall: func(n int) []gitlab.CommitStatus {
		if n < 2 {
			return nil
		}
		return goodStatus
	}}
	c := &fakeCaller{statuses: []string{"pending", "planning", "planned"}}
	st, err := Wait(context.Background(), ss, c, testSource, testWaits, newClock())
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != PhaseFinished || st.Status != "planned" || st.PlanID != "plan-xyz" || st.Ref.RunID != "run-GOOD1" || st.Ref.Workspace != "net-prod" {
		t.Fatalf("state = %+v", st)
	}
	if c.gotName != ToolGetRunDetails || c.gotArgs["run_id"] != "run-GOOD1" {
		t.Fatalf("call = %s %v", c.gotName, c.gotArgs)
	}
	if c.n != 3 {
		t.Fatalf("polls = %d", c.n)
	}
}

func TestWaitFailedPhase(t *testing.T) {
	ss := &fakeStatuses{byCall: func(int) []gitlab.CommitStatus { return goodStatus }}
	c := &fakeCaller{statuses: []string{"errored"}}
	st, err := Wait(context.Background(), ss, c, testSource, testWaits, newClock())
	if err != nil || st.Phase != PhaseFailed {
		t.Fatalf("state=%+v err=%v", st, err)
	}
}

func TestWaitTotalTimeout(t *testing.T) {
	ss := &fakeStatuses{byCall: func(int) []gitlab.CommitStatus { return goodStatus }}
	c := &fakeCaller{statuses: []string{"some_unknown_status"}}
	_, err := Wait(context.Background(), ss, c, testSource, testWaits, newClock())
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v", err)
	}
}

func TestWaitContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ss := &fakeStatuses{byCall: func(int) []gitlab.CommitStatus { return goodStatus }}
	c := &fakeCaller{statuses: []string{"planning"}}
	clk := newClock()
	clk.onSlp = func(n int) {
		if n == 2 {
			cancel()
		}
	}
	_, err := Wait(ctx, ss, c, testSource, testWaits, clk)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestWaitStatusSourceErrorWrapped(t *testing.T) {
	boom := errors.New("boom")
	ss := &fakeStatuses{byCall: func(int) []gitlab.CommitStatus { return nil }, err: boom}
	_, err := Wait(context.Background(), ss, &fakeCaller{}, testSource, testWaits, newClock())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestWaitCallerErrorWrapped(t *testing.T) {
	boom := errors.New("boom")
	ss := &fakeStatuses{byCall: func(int) []gitlab.CommitStatus { return goodStatus }}
	_, err := Wait(context.Background(), ss, &fakeCaller{err: boom}, testSource, testWaits, newClock())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestRealClock(t *testing.T) {
	var c Clock = RealClock{}
	if c.Now().IsZero() {
		t.Fatal("zero now")
	}
	if err := c.Sleep(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestPhaseOf(t *testing.T) {
	cases := map[string]Phase{
		"pending": PhaseQueued, "plan_queued": PhaseQueued, "queuing": PhaseQueued,
		"planning": PhaseRunning, "fetching": PhaseRunning, "pre_plan_running": PhaseRunning, "weird": PhaseRunning,
		"planned": PhaseFinished, "planned_and_finished": PhaseFinished, "cost_estimated": PhaseFinished, "policy_checked": PhaseFinished,
		"errored": PhaseFailed, "canceled": PhaseFailed, "discarded": PhaseFailed, "force_canceled": PhaseFailed,
	}
	for s, want := range cases {
		if got := phaseOf(s); got != want {
			t.Errorf("phaseOf(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestVerifyRunSHA(t *testing.T) {
	if err := VerifyRunSHA(runJSON("planned", "p", "abc123"), "abc123"); err != nil {
		t.Fatalf("match: %v", err)
	}
	err := VerifyRunSHA(runJSON("planned", "p", "SECRETSHA"), "abc123")
	if err == nil || strings.Contains(err.Error(), "SECRETSHA") {
		t.Fatalf("mismatch err = %v", err)
	}
}

func TestVerifyRunSHAIncluded(t *testing.T) {
	js := `{"data":{"attributes":{"status":"planned"}},"included":[{"type":"ingress-attributes","attributes":{"commit-sha":"abc123"}}]}`
	if err := VerifyRunSHA(js, "abc123"); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRunSHAFailsClosed(t *testing.T) {
	for _, js := range []string{`{"data":{"attributes":{"status":"planned"}}}`, `not json SECRETPAYLOAD`, ``} {
		err := VerifyRunSHA(js, "abc123")
		if err == nil || strings.Contains(err.Error(), "SECRETPAYLOAD") {
			t.Errorf("js=%q err=%v", js, err)
		}
	}
	if err := VerifyRunSHA(runJSON("planned", "p", "abc123"), ""); err == nil {
		t.Error("empty wantSHA must fail")
	}
}
