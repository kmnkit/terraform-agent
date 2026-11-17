package llm

import (
	"strings"
	"testing"

	"github.com/terraform-agent/internal/model"
)

func sampleInput() ReviewInput {
	return ReviewInput{
		Resources: []ReviewResource{{
			Address: "google_storage_bucket.logs", File: "main.tf",
			ChangedAttrs: []string{"location"}, DiffExcerpt: "+ location = \"ASIA\"",
			DocExcerpt: "DOC-EXCERPT-SENTINEL", DocTruncated: true,
		}},
		Modules:       []ModuleContext{{Name: "net", Source: "terraform-google-modules/network/google", Version: "9.0.0"}},
		Checks:        []model.CheckResult{{Tool: "tflint", Passed: false, Summary: "CHECK-SUMMARY-SENTINEL"}},
		DiffTruncated: true,
	}
}

func TestBuildReviewPrompt_ContainsInputs(t *testing.T) {
	p := BuildReviewPrompt(sampleInput())
	for _, want := range []string{
		"google_storage_bucket.logs", "main.tf", "location", "+ location = \"ASIA\"",
		"DOC-EXCERPT-SENTINEL", "CHECK-SUMMARY-SENTINEL", "tflint",
		"terraform-google-modules/network/google", "9.0.0", "한국어", "문서 근거 없음",
		"데이터", "<<<",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
}

func TestBuildReviewPrompt_NoDocSaysNoEvidence(t *testing.T) {
	in := sampleInput()
	in.Resources[0].DocExcerpt = ""
	if !strings.Contains(BuildReviewPrompt(in), "문서 근거 없음") {
		t.Error("expected no-evidence marker")
	}
}

func TestBuildReviewPrompt_OnlyEmitsReviewInputFields(t *testing.T) {
	in := ReviewInput{Resources: []ReviewResource{{Address: "a.b"}}}
	p := BuildReviewPrompt(in)
	for _, banned := range []string{"tfvars", "SECRET-VALUE-SENTINEL"} {
		if strings.Contains(p, banned) {
			t.Errorf("prompt unexpectedly contains %q", banned)
		}
	}
}

func TestBuildPlanRisksPrompt_AddressesActionsAttrNamesOnly(t *testing.T) {
	s := model.PlanSummary{Destroy: 1, Targets: []model.Target{
		{Address: "google_sql_database_instance.main", Action: "delete", Attrs: []string{"deletion_protection"}},
	}}
	p := BuildPlanRisksPrompt(s)
	for _, want := range []string{"google_sql_database_instance.main", "delete", "deletion_protection", "한국어"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	for _, banned := range []string{"VALUE-SENTINEL", "password", "tfvars"} {
		if strings.Contains(p, banned) {
			t.Errorf("prompt contains %q", banned)
		}
	}
}

func TestBuildPlanFailurePrompt(t *testing.T) {
	p := BuildPlanFailurePrompt([]string{"Error: quota exceeded", "Error: api disabled"})
	for _, want := range []string{"Error: quota exceeded", "Error: api disabled", "한국어", "데이터"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
}

func TestParseFindings(t *testing.T) {
	known := map[string]bool{"a.x": true, "b.y": true}
	raw := `{"findings":[
	  {"resource":"a.x","severity":"warning","message":"  첫째  "},
	  {"resource":"zzz.unknown","severity":"error","message":"지어낸 리소스"},
	  {"resource":"b.y","severity":"CRITICAL","message":"등급 이상"},
	  {"resource":"b.y","severity":"info","message":"   "}]}`
	got, err := ParseFindings(raw, known)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d findings: %+v", len(got), got)
	}
	if got[0] != (model.Finding{Resource: "a.x", Severity: "warning", Message: "첫째"}) {
		t.Errorf("first: %+v", got[0])
	}
	if got[1].Severity != "info" || got[1].DocRef != "" {
		t.Errorf("second: %+v", got[1])
	}
}

func TestParseFindings_IgnoresLLMDocRefAndCaps(t *testing.T) {
	known := map[string]bool{"a.x": true}
	got, err := ParseFindings(`{"findings":[{"resource":"a.x","severity":"info","message":"m","docRef":"http://evil"}]}`, known)
	if err != nil || got[0].DocRef != "" {
		t.Fatalf("got %+v err %v", got, err)
	}
	var sb strings.Builder
	sb.WriteString(`{"findings":[`)
	for i := 0; i < maxFindings+10; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"resource":"a.x","severity":"info","message":"m"}`)
	}
	sb.WriteString(`]}`)
	got, err = ParseFindings(sb.String(), known)
	if err != nil || len(got) != maxFindings {
		t.Fatalf("len %d err %v", len(got), err)
	}
}

func TestParse_Malformed(t *testing.T) {
	if _, err := ParseFindings("not json", nil); err == nil {
		t.Error("findings: want error")
	}
	if _, err := ParseRisks("{"); err == nil {
		t.Error("risks: want error")
	}
	if _, err := ParseFailure(""); err == nil {
		t.Error("failure: want error")
	}
}

func TestParseRisks(t *testing.T) {
	got, err := ParseRisks(`{"risks":[" 삭제 위험 ","","재생성"]}`)
	if err != nil || len(got) != 2 || got[0] != "삭제 위험" {
		t.Fatalf("got %v err %v", got, err)
	}
}

func TestParseFailure(t *testing.T) {
	got, err := ParseFailure(`{"analysis":"  API 비활성  "}`)
	if err != nil || got != "API 비활성" {
		t.Fatalf("got %q err %v", got, err)
	}
	if _, err := ParseFailure(`{"analysis":"  "}`); err == nil {
		t.Error("empty analysis: want error")
	}
}
