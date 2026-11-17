package staticcheck

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/terraform-agent/internal/model"
)

func TestArtifactName(t *testing.T) {
	if ArtifactName != "tf-review-staticcheck.json" {
		t.Fatalf("ArtifactName = %q", ArtifactName)
	}
}

func TestJobName(t *testing.T) {
	if JobName != "tf-review-staticcheck" {
		t.Fatalf("JobName = %q", JobName)
	}
}

func TestParse_AllTools(t *testing.T) {
	got, err := Parse(readTestdata(t, "all_tools.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []model.CheckResult{
		{Tool: "fmt", Passed: true, Summary: "formatted"},
		{Tool: "validate", Passed: true, Summary: "valid"},
		{Tool: "tflint", Passed: true, Summary: "no issues"},
		{Tool: "trivy", Passed: false, Summary: "1 HIGH finding"},
	}
	assertResults(t, got, want)
}

func TestParse_MissingToolIsReportedAsUnknown(t *testing.T) {
	raw := `{"version":1,"results":[{"tool":"fmt","passed":true,"summary":"ok"}]}`
	got, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []model.CheckResult{
		{Tool: "fmt", Passed: true, Summary: "ok"},
		{Tool: "validate", Passed: false, Summary: SummaryUnknown},
		{Tool: "tflint", Passed: false, Summary: SummaryUnknown},
		{Tool: "trivy", Passed: false, Summary: SummaryUnknown},
	}
	assertResults(t, got, want)
	if SummaryUnknown != "unknown" {
		t.Fatalf("SummaryUnknown = %q", SummaryUnknown)
	}
}

func TestParse_EmptyArtifact(t *testing.T) {
	for _, raw := range []string{"", "  \n\t "} {
		got, err := Parse([]byte(raw))
		if err != nil {
			t.Fatalf("Parse(%q): %v", raw, err)
		}
		if len(got) != 0 {
			t.Fatalf("Parse(%q) = %v, want empty", raw, got)
		}
	}
}

func TestParse_RejectsInvalidInput(t *testing.T) {
	cases := map[string]string{
		"malformed":           `{"version":1,`,
		"unknown field":       `{"version":1,"results":[],"extra":1}`,
		"unknown entry field": `{"version":1,"results":[{"tool":"fmt","passed":true,"summary":"","x":1}]}`,
		"unknown tool":        `{"version":1,"results":[{"tool":"checkov","passed":true,"summary":""}]}`,
		"duplicate":           `{"version":1,"results":[{"tool":"fmt","passed":true,"summary":""},{"tool":"fmt","passed":false,"summary":""}]}`,
		"trailing data":       `{"version":1,"results":[]} {}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(raw)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestParse_RejectsUnknownVersion(t *testing.T) {
	for _, raw := range []string{`{"version":2,"results":[]}`, `{"results":[]}`} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Fatalf("Parse(%s): expected error", raw)
		}
	}
	_, err := Parse([]byte(`{"version":7,"results":[]}`))
	if err == nil || !strings.Contains(err.Error(), "7") {
		t.Fatalf("error should name version 7, got %v", err)
	}
}

func TestParse_RejectsOversizedInput(t *testing.T) {
	raw := make([]byte, MaxArtifactBytes+1)
	if _, err := Parse(raw); err == nil {
		t.Fatal("expected error for oversized input")
	}
}

func TestParse_TruncatesLongSummary(t *testing.T) {
	long := strings.Repeat("한", MaxSummaryRunes+500)
	raw := `{"version":1,"results":[{"tool":"fmt","passed":false,"summary":"` + long + `"}]}`
	got, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	s := got[0].Summary
	if n := utf8.RuneCountInString(s); n != MaxSummaryRunes {
		t.Fatalf("rune count = %d, want %d", n, MaxSummaryRunes)
	}
	if !utf8.ValidString(s) || !strings.HasSuffix(s, "…") {
		t.Fatal("summary invalid or missing marker")
	}
}

func TestParse_KeepsSummaryAtCap(t *testing.T) {
	exact := strings.Repeat("a", MaxSummaryRunes)
	raw := `{"version":1,"results":[{"tool":"fmt","passed":true,"summary":"` + exact + `"}]}`
	got, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got[0].Summary != exact {
		t.Fatal("summary at cap must not be truncated")
	}
}
