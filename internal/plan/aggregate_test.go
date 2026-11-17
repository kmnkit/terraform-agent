package plan

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/terraform-agent/internal/model"
)

func loadPlan(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/plan_with_sensitive.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAggregateNoValuesLeak(t *testing.T) {
	sum, err := Aggregate(loadPlan(t), 100)
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(sum)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"SUPERSECRET-PRIOR", "SUPERSECRET-BEFORE", "SUPERSECRET-AFTER", "SUPERSECRET-VAR", "SUPERSECRET-KEY"} {
		if strings.Contains(string(out), s) {
			t.Fatalf("summary leaks %s: %s", s, out)
		}
	}
}

func TestAggregateCountsAndTargets(t *testing.T) {
	sum, err := Aggregate(loadPlan(t), 100)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Add != 1 || sum.Change != 1 || sum.Destroy != 1 || sum.Replace != 2 || sum.Truncated {
		t.Fatalf("counts = %+v", sum)
	}
	want := []model.Target{
		{Address: "google_compute_disk.d", Action: "replace", Attrs: []string{"id", "size"}},
		{Address: "google_compute_instance.old", Action: "delete", Attrs: []string{"metadata", "name"}},
		{Address: "google_compute_network.n", Action: "replace", Attrs: []string{"name"}},
		{Address: "google_sql_user.u", Action: "update", Attrs: []string{"password"}},
		{Address: "google_storage_bucket.logs", Action: "create", Attrs: []string{"id", "labels", "location", "name", "url"}},
	}
	if len(sum.Targets) != len(want) {
		t.Fatalf("targets = %+v", sum.Targets)
	}
	for i, w := range want {
		g := sum.Targets[i]
		if g.Address != w.Address || g.Action != w.Action || strings.Join(g.Attrs, ",") != strings.Join(w.Attrs, ",") {
			t.Errorf("target[%d] = %+v, want %+v", i, g, w)
		}
	}
}

func TestAggregateTruncated(t *testing.T) {
	sum, err := Aggregate(loadPlan(t), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Targets) != 2 || !sum.Truncated {
		t.Fatalf("targets=%d truncated=%v", len(sum.Targets), sum.Truncated)
	}
	if sum.Add != 1 || sum.Change != 1 || sum.Destroy != 1 || sum.Replace != 2 {
		t.Fatalf("counts must reflect all targets: %+v", sum)
	}
}

func TestAggregateInvalidJSONErrorHasNoPayload(t *testing.T) {
	_, err := Aggregate([]byte(`{"resource_changes": SUPERSECRET-X`), 5)
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "SUPERSECRET") {
		t.Fatalf("error leaks payload: %v", err)
	}
}

func TestAggregateNegativeMaxTargets(t *testing.T) {
	sum, err := Aggregate(loadPlan(t), -1)
	if err != nil || len(sum.Targets) != 0 || !sum.Truncated || sum.Add != 1 {
		t.Fatalf("sum=%+v err=%v", sum, err)
	}
}

func TestAggregateNestedAfterUnknownAndNumbers(t *testing.T) {
	js := `{"resource_changes":[{"address":"a.b","change":{"actions":["update"],
	  "before":{"big":12345678901234567890,"same":{"x":[1,2]}},
	  "after":{"big":12345678901234567891,"same":{"x":[1,2]}},
	  "after_unknown":{"nested":{"deep":[{"id":true}]},"known":{"x":false},"top":true}}}]}`
	sum, err := Aggregate([]byte(js), 5)
	if err != nil || len(sum.Targets) != 1 {
		t.Fatalf("sum=%+v err=%v", sum, err)
	}
	got := strings.Join(sum.Targets[0].Attrs, ",")
	if got != "big,nested,top" {
		t.Fatalf("attrs = %s, want big,nested,top", got)
	}
}

func TestAggregateEmpty(t *testing.T) {
	sum, err := Aggregate([]byte(`{}`), 5)
	if err != nil || sum.Add+sum.Change+sum.Destroy+sum.Replace != 0 || len(sum.Targets) != 0 {
		t.Fatalf("sum=%+v err=%v", sum, err)
	}
}
