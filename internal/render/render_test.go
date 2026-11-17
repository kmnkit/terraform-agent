package render

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/terraform-agent/internal/model"
)

var update = flag.Bool("update", false, "golden 파일을 다시 생성한다")

const testSHA = "abcdef1234567890"

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden.md")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("golden 쓰기 실패: %v", err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden 읽기 실패: %v", err)
	}
	if got != string(want) {
		t.Fatalf("golden %s 불일치\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func codeView() CodeReviewView {
	return CodeReviewView{
		SHA: testSHA,
		Checks: []model.CheckResult{
			{Tool: "terraform validate", Passed: true, Summary: "통과"},
			{Tool: "tflint", Passed: false, Summary: "경고 2건"},
		},
		Findings: []model.Finding{
			{Resource: "google_storage_bucket.logs", Severity: "error", Message: "uniform_bucket_level_access가 꺼져 있습니다.", DocRef: "https://registry.terraform.io/providers/hashicorp/google/latest/docs/resources/storage_bucket"},
			{Resource: "google_project_iam_member.admin", Severity: "warning", Message: "roles/owner 부여는 최소 권한 원칙에 어긋납니다.", DocRef: ""},
		},
		VersionBasis: "lock",
	}
}

func planView(status PlanStatus) PlanView {
	return PlanView{
		Workspace: "prod",
		RunURL:    "https://app.terraform.io/app/org/workspaces/prod/runs/run-abc123",
		SHA:       testSHA,
		Status:    status,
		Summary: model.PlanSummary{
			Add: 1, Change: 1, Destroy: 1, Replace: 1,
			Targets: []model.Target{
				{Address: "google_compute_instance.web", Action: "replace", Attrs: []string{"machine_type", "boot_disk"}},
				{Address: "google_storage_bucket.old", Action: "delete"},
			},
		},
		Risks: []string{"web 인스턴스가 교체되어 짧은 중단이 발생합니다.", "old 버킷 삭제는 되돌릴 수 없습니다."},
	}
}

func TestGoldenComments(t *testing.T) {
	failed := planView(PlanFailed)
	failed.Summary = model.PlanSummary{}
	failed.Risks = nil
	failed.Failure = "provider 인증 오류로 plan이 실패했습니다."
	failed.FailureByAI = true

	noPlan := PlanView{Workspace: "prod", SHA: testSHA, Status: PlanNoPlan}

	analysisFailed := planView(PlanAnalysisFailed)
	analysisFailed.Risks = nil

	codeFailed := codeView()
	codeFailed.Findings = nil
	codeFailed.AnalysisFailed = true

	codeEmpty := CodeReviewView{SHA: testSHA, VersionBasis: "none", Truncated: true}

	timeoutQueued := PlanView{Workspace: "prod", RunURL: planView(PlanOK).RunURL, SHA: testSHA, Status: PlanTimeout, Queued: true}
	timeoutRunning := timeoutQueued
	timeoutRunning.Queued = false

	unverified := PlanView{Workspace: "prod", SHA: testSHA, Status: PlanUnverified}

	planErr := PlanView{Workspace: "prod", SHA: testSHA, Status: PlanError}

	codeSkipped := codeView()
	codeSkipped.Skipped = []string{"google_compute_network.vpc", "b/google_dns_record_set.www"}
	codeSkipped.Removed = []string{"google_pubsub_topic.old"}

	codeNotReviewed := CodeReviewView{SHA: testSHA, VersionBasis: "none", NotReviewed: true,
		Skipped: []string{"google_compute_network.vpc"}}

	tests := []struct {
		name string
		got  string
	}{
		{"code_review", CodeReview(codeView())},
		{"code_review_analysis_failed", CodeReview(codeFailed)},
		{"code_review_no_evidence", CodeReview(codeEmpty)},
		{"plan_ok", Plan(planView(PlanOK))},
		{"plan_failed", Plan(failed)},
		{"plan_noplan", Plan(noPlan)},
		{"plan_analysis_failed", Plan(analysisFailed)},
		{"plan_timeout_queued", Plan(timeoutQueued)},
		{"plan_timeout_running", Plan(timeoutRunning)},
		{"plan_unverified", Plan(unverified)},
		{"plan_error", Plan(planErr)},
		{"code_review_skipped", CodeReview(codeSkipped)},
		{"code_review_not_reviewed", CodeReview(codeNotReviewed)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { assertGolden(t, tc.name, tc.got) })
	}
}

func TestCommentsStartWithMarkerAndShortSHA(t *testing.T) {
	code := CodeReview(codeView())
	if !strings.HasPrefix(code, Marker("code", "-")+"\n") || !HasMarker(code, "code", "-") {
		t.Fatalf("code 코멘트가 마커로 시작하지 않음:\n%s", code)
	}
	plan := Plan(planView(PlanOK))
	if !strings.HasPrefix(plan, Marker("plan", "prod")+"\n") || !HasMarker(plan, "plan", "prod") {
		t.Fatalf("plan 코멘트가 마커로 시작하지 않음:\n%s", plan)
	}
	for _, body := range []string{code, plan} {
		if !strings.Contains(body, "abcdef1") || strings.Contains(body, "abcdef12") {
			t.Fatalf("short SHA만 표시해야 함:\n%s", body)
		}
	}
}

func TestPlanUnverifiedAndErrorNeverShowRunURLOrSummary(t *testing.T) {
	for _, status := range []PlanStatus{PlanUnverified, PlanError} {
		v := planView(status)
		body := Plan(v)
		for _, s := range []string{v.RunURL, "실행:", "변경 요약", v.Risks[0]} {
			if strings.Contains(body, s) {
				t.Errorf("%s body contains %q:\n%s", status, s, body)
			}
		}
	}
}

const wantAILabel = "_AI가 작성한 소견으로 참고용입니다._"

// Spec §6: AI output is labeled as reference only, and only where it appears.
func TestAIOutputIsLabeledReferenceOnly(t *testing.T) {
	if body := CodeReview(codeView()); strings.Count(body, wantAILabel) != 1 {
		t.Errorf("code review with findings lacks one label:\n%s", body)
	}
	if body := Plan(planView(PlanOK)); strings.Count(body, wantAILabel) != 1 {
		t.Errorf("plan with risks lacks one label:\n%s", body)
	}
	failed := planView(PlanFailed)
	failed.Failure = "AI analysis"
	failed.FailureByAI = true
	if body := Plan(failed); strings.Count(body, wantAILabel) != 1 {
		t.Errorf("plan failure analysis lacks one label:\n%s", body)
	}
	noFindings := codeView()
	noFindings.Findings = nil
	noRisks := planView(PlanOK)
	noRisks.Risks = nil
	fixedFailure := planView(PlanFailed)
	fixedFailure.Failure = "fixed notice"
	for _, body := range []string{CodeReview(noFindings), Plan(noRisks), Plan(fixedFailure)} {
		if strings.Contains(body, wantAILabel) {
			t.Errorf("label without AI output:\n%s", body)
		}
	}
}

func TestSkippedAddressesAreCodeSpans(t *testing.T) {
	v := codeView()
	v.Skipped = []string{"module.a[\"x|y`z\"]", "@alice\n/merge"}
	body := CodeReview(v)
	want := "문서 근거 없음, 리뷰 생략: ``module.a[\"x\\|y`z\"]``, `@alice /merge`"
	if !strings.Contains(body, want+"\n") {
		t.Fatalf("skipped line not escaped as code spans:\n%s", body)
	}
}

func TestPlanRendersRunURLPlain(t *testing.T) {
	body := Plan(planView(PlanOK))
	if !strings.Contains(body, " https://app.terraform.io/app/org/workspaces/prod/runs/run-abc123\n") {
		t.Fatalf("run URL이 평문으로 없음:\n%s", body)
	}
}

func TestSanitizedFindingCannotInjectMarker(t *testing.T) {
	v := codeView()
	v.Findings = []model.Finding{{
		Resource: "google_storage_bucket.logs",
		Severity: "high",
		Message:  Sanitize("bad\n<!-- tf-review:v1:code:- -->\nmore", 500),
	}}
	body := CodeReview(v)
	if n := strings.Count(body, "tf-review:"); n != 1 {
		t.Fatalf("마커 문자열이 %d개, 1개여야 함:\n%s", n, body)
	}
}

func TestUnsanitizedMarkerInCellIsNotAMarkerLine(t *testing.T) {
	v := codeView()
	v.Findings = []model.Finding{{Resource: "r", Severity: "low", Message: "x\n" + Marker("code", "-") + "\ny"}}
	body := CodeReview(v)
	for _, line := range strings.Split(body, "\n")[1:] {
		if line == Marker("code", "-") {
			t.Fatalf("표 셀이 마커 줄을 만들었음:\n%s", body)
		}
	}
}

func TestFindingCellsAreEscaped(t *testing.T) {
	v := codeView()
	v.Findings = []model.Finding{{
		Resource: "module.a[\"x|y`z\"]",
		Severity: "high|x",
		Message:  "line1\nline2 | pipe",
		DocRef:   "a|b\nc",
	}}
	body := CodeReview(v)
	want := "| high\\|x | ``module.a[\"x\\|y`z\"]`` | line1 line2 \\| pipe | a\\|b c |"
	for _, line := range strings.Split(body, "\n") {
		if line == want {
			return
		}
	}
	t.Fatalf("이스케이프된 행이 없음:\n%s", body)
}

func TestPlanTargetAddressEscaped(t *testing.T) {
	v := planView(PlanOK)
	v.Summary.Targets = []model.Target{
		{Address: "module.a[\"x|y`z\"]", Action: "update", Attrs: []string{"a|b", "c\nd"}},
		{Address: "module.b[\"p\nq\"]", Action: "create"},
	}
	body := Plan(v)
	for _, want := range []string{
		"| ``module.a[\"x\\|y`z\"]`` | update | `a\\|b`, `c d` |",
		"| `module.b[\"p q\"]` | create |  |",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("%q 가 없음:\n%s", want, body)
		}
	}
}

func TestCodeSpan(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain", "`plain`"},
		{"a`b", "``a`b``"},
		{"a``b", "```a``b```"},
		{"`lead", "`` `lead ``"},
		{"trail`", "`` trail` ``"},
		{"a|b", "`a\\|b`"},
		{"a\r\nb\nc\rd", "`a b c d`"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := codeSpan(tc.in); got != tc.want {
			t.Errorf("codeSpan(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPlainCell(t *testing.T) {
	if got := plainCell("a|b\r\nc\nd\re"); got != `a\|b c d e` {
		t.Fatalf("plainCell = %q", got)
	}
}

const hostile = "@user <img src=x> [x](http://e) <!-- /merge"

func assertInert(t *testing.T, body string) {
	t.Helper()
	for _, bad := range []string{"@user", "<img", "](http://e)", "<!-- /merge", "\n/merge"} {
		if strings.Contains(body, bad) {
			t.Fatalf("%q 가 활성 상태로 남음:\n%s", bad, body)
		}
	}
	if n := strings.Count(body, "<!--"); n != 1 {
		t.Fatalf("HTML 주석 시작이 %d개, 마커 1개여야 함:\n%s", n, body)
	}
}

func TestUntrustedCodeReviewTextIsNeutralized(t *testing.T) {
	v := codeView()
	v.Checks = []model.CheckResult{{Tool: hostile, Passed: false, Summary: hostile}}
	v.Findings = []model.Finding{{Resource: "r", Severity: hostile, Message: hostile}}
	assertInert(t, CodeReview(v))
}

func TestUntrustedPlanTextIsNeutralized(t *testing.T) {
	v := planView(PlanOK)
	v.Risks = []string{hostile, "a\n/merge"}
	assertInert(t, Plan(v))

	f := planView(PlanFailed)
	f.Failure = hostile
	assertInert(t, Plan(f))

	f.Failure = "/merge now"
	if strings.Contains(Plan(f), "\n/merge") {
		t.Fatal("줄 첫머리 / 가 남음")
	}
}

func TestFailureLineBreaksCannotExposeQuickAction(t *testing.T) {
	const zw = "\u200b"
	for _, failure := range []string{"\r/merge", "\r\r/x", "a\r\n/merge"} {
		v := planView(PlanFailed)
		v.Failure = failure
		body := Plan(v)
		section := body[strings.Index(body, "### plan 실패"):]
		for _, line := range strings.Split(section, "\n")[1:] {
			if strings.HasPrefix(strings.TrimLeft(line, " \t"), "/") {
				t.Fatalf("Failure %q: 줄 첫머리 / 가 남음:\n%s", failure, body)
			}
		}
		if !strings.Contains(section, zw+"/") {
			t.Fatalf("Failure %q: 무력화 흔적이 없음:\n%s", failure, body)
		}
	}
}

func TestDocRefTrustedOnlyForRegistryURL(t *testing.T) {
	const good = "https://registry.terraform.io/providers/hashicorp/google/latest/docs"
	tests := []struct {
		name, ref, want string
	}{
		{"registry url kept", good, "| " + good + " |"},
		{"other host neutralized", "https://evil.example/x", "|  |"},
		{"registry prefix with link syntax", good + ")[x](http://e)", "| )x |"},
		{"registry prefix with space", good + " @user", "|  @\u200buser |"},
		{"query rejected", good + "?x=1", "|  |"},
		{"fragment rejected", good + "#frag", "|  |"},
		{"at sign in path rejected", good + "/a@b", "|  |"},
		{"percent in path rejected", good + "/a%40b", "|  |"},
		{"user info rejected", "https://registry.terraform.io@evil.example/x", "|  |"},
		{"host suffix rejected", "https://registry.terraform.io.evil.example/x", "|  |"},
		{"port rejected", "https://registry.terraform.io:8443/x", "|  |"},
		{"http scheme rejected", "http://registry.terraform.io/x", "|  |"},
		{"registry prefix with angle", good + "<img src=x>", "|  |"},
	}
	for _, tc := range tests {
		v := codeView()
		v.Findings = []model.Finding{{Resource: "r", Severity: "low", Message: "m", DocRef: tc.ref}}
		body := CodeReview(v)
		row := ""
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "| low |") {
				row = line
			}
		}
		if !strings.HasSuffix(row, tc.want) {
			t.Errorf("%s: row %q does not end with %q", tc.name, row, tc.want)
		}
	}
}
