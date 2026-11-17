package plan

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/terraform-agent/internal/gitlab"
)

type statusFixture struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	TargetURL string `json:"target_url"`
	SHA       string `json:"sha"`
	OK        bool   `json:"ok"`
	Run       string `json:"run"`
	WS        string `json:"ws"`
}

func (f statusFixture) status() gitlab.CommitStatus {
	return gitlab.CommitStatus{Name: f.Name, Status: f.Status, TargetURL: f.TargetURL, SHA: f.SHA}
}

func loadStatuses(t *testing.T) []statusFixture {
	t.Helper()
	b, err := os.ReadFile("testdata/statuses.json")
	if err != nil {
		t.Fatal(err)
	}
	var out []statusFixture
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

var testSource = Source{Host: "app.terraform.io", Org: "acme", Workspaces: []string{"net-prod", "net-dev"}}

func TestFindRunEachFixture(t *testing.T) {
	for _, f := range loadStatuses(t) {
		t.Run(f.Name, func(t *testing.T) {
			ref, ok := FindRun([]gitlab.CommitStatus{f.status()}, testSource)
			if ok != f.OK {
				t.Fatalf("ok = %v, want %v (ref %+v)", ok, f.OK, ref)
			}
			if ok && (ref.RunID != f.Run || ref.Workspace != f.WS || ref.StatusState != f.Status) {
				t.Fatalf("ref = %+v", ref)
			}
		})
	}
}

func TestFindRunFirstMatchWinsAmongForged(t *testing.T) {
	var all []gitlab.CommitStatus
	for _, f := range loadStatuses(t) {
		all = append(all, f.status())
	}
	ref, ok := FindRun(all, testSource)
	if !ok || ref.RunID != "run-GOOD1" {
		t.Fatalf("ref=%+v ok=%v", ref, ok)
	}
}

func TestFindRunNone(t *testing.T) {
	if _, ok := FindRun(nil, testSource); ok {
		t.Fatal("want no match")
	}
}
