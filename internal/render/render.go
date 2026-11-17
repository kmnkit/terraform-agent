package render

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"text/template"

	"github.com/terraform-agent/internal/model"
)

const (
	codeKind  = "code"
	planKind  = "plan"
	codeScope = "-"
	shortSHAN = 7
)

// PlanStatus selects which variant of the plan comment is rendered.
type PlanStatus string

const (
	PlanOK             PlanStatus = "ok"
	PlanFailed         PlanStatus = "failed"
	PlanNoPlan         PlanStatus = "noplan"
	PlanAnalysisFailed PlanStatus = "analysisFailed"
	PlanTimeout        PlanStatus = "timeout"
	// PlanUnverified: the linked run could not be bound to this commit. It
	// shows a fixed notice only, never a RunURL or summary.
	PlanUnverified PlanStatus = "unverified"
	// PlanError: a stage of the plan analysis failed (spec §5). It shows a
	// fixed analysis-failed notice only, never a RunURL or summary.
	PlanError PlanStatus = "error"
)

// CodeReviewView is the data for a code review comment. render neutralizes all
// untrusted text itself (check output, finding text); Resource and Skipped
// addresses stay in code spans and DocRef is emitted as a plain cell.
type CodeReviewView struct {
	SHA            string
	Checks         []model.CheckResult
	Findings       []model.Finding
	Truncated      bool
	VersionBasis   string // "lock", "constraint" or "none"
	AnalysisFailed bool
	// Skipped are the addresses of changed resources without documentation
	// evidence; they were not reviewed (spec §3).
	Skipped []string
	// Removed are the addresses of deleted resources; they get no doc lookup
	// and no AI review.
	Removed []string
	// NotReviewed: no resource had documentation evidence, so no AI review ran.
	NotReviewed bool
}

// PlanView is the data for a plan comment. render neutralizes Risks, Failure
// and other text itself; RunURL is a trusted value and is emitted as is.
type PlanView struct {
	Workspace string
	RunURL    string
	SHA       string
	Status    PlanStatus
	Summary   model.PlanSummary
	Risks     []string
	Failure   string
	// FailureByAI: Failure was written by the LLM, so it carries the
	// reference-only label (spec §6); fixed notices do not.
	FailureByAI bool
	// Queued is used by PlanTimeout only: the last observed run phase was
	// queued (true) or running (false).
	Queued bool
}

var funcs = template.FuncMap{
	"marker":  Marker,
	"short":   shortSHA,
	"code":    codeSpan,
	"cell":    plainCell,
	"text":    safeCell,
	"docref":  docRefCell,
	"line":    safeLine,
	"attrs":   attrList,
	"passed":  passedLabel,
	"basis":   basisNote,
	"is":      func(got, want PlanStatus) bool { return got == want },
	"ailabel": func() string { return aiLabel },
}

// aiLabel marks LLM output as reference only (spec §6).
const aiLabel = "_AI가 작성한 소견으로 참고용입니다._"

var codeTmpl = template.Must(template.New(codeKind).Funcs(funcs).Parse(`{{marker "code" "-"}}
## Terraform 코드 리뷰

기준 커밋: {{code (short .SHA)}}
{{- if .AnalysisFailed}}

> 분석 실패: 자동 분석을 완료하지 못했습니다. 정적 검사 결과만 표시합니다.
{{- end}}
{{- if .Truncated}}

> 변경이 커서 일부만 검토했습니다.
{{- end}}

### 정적 검사
{{if .Checks}}
| 도구 | 결과 | 요약 |
| --- | --- | --- |
{{range .Checks -}}
| {{text .Tool}} | {{passed .Passed}} | {{text .Summary}} |
{{end -}}
{{else}}
정적 검사 결과 없음
{{end}}
### 리뷰 소견
{{if .Findings}}
{{ailabel}}

| 심각도 | 리소스 | 내용 | 문서 |
| --- | --- | --- | --- |
{{range .Findings -}}
| {{text .Severity}} | {{code .Resource}} | {{text .Message}} | {{docref .DocRef}} |
{{end -}}
{{else if .AnalysisFailed}}
소견을 생성하지 못했습니다.
{{else if .NotReviewed}}
문서 근거가 있는 리소스가 없어 AI 리뷰를 하지 않았습니다.
{{else}}
발견 사항 없음
{{end}}
{{- if .Skipped}}
문서 근거 없음, 리뷰 생략: {{attrs .Skipped}}
{{end}}
{{- if .Removed}}
삭제된 리소스, 문서 조회 대상 아님: {{attrs .Removed}}
{{end}}
{{basis .VersionBasis}}
`))

var planTmpl = template.Must(template.New(planKind).Funcs(funcs).Parse(`{{marker "plan" .Workspace}}
## Terraform plan 분석

workspace: {{code .Workspace}}
기준 커밋: {{code (short .SHA)}}
{{- if and .RunURL (not (is .Status "unverified")) (not (is .Status "error"))}}
실행: {{.RunURL}}
{{- end}}
{{if is .Status "unverified"}}
> 이 커밋의 plan을 확인하지 못했습니다. 연결된 HCP Terraform run을 이 커밋의 run으로 확인할 수 없습니다.
{{- else if is .Status "error"}}
> 분석 실패: 이 커밋의 plan을 분석하지 못했습니다. HCP Terraform 실행 화면에서 확인하세요.
{{- else if is .Status "noplan"}}
> 이 커밋에 해당하는 plan을 찾지 못했습니다.
{{- else if is .Status "timeout"}}
> 대기 시간 안에 plan이 끝나지 않았습니다. 마지막으로 확인한 상태는 {{if .Queued}}대기 중(queued){{else}}실행 중(running){{end}}입니다.
{{- else}}
{{- if is .Status "analysisFailed"}}
> 분석 실패: plan 요약만 표시합니다. 위험 분석은 제공되지 않습니다.
{{end}}
{{- if is .Status "failed"}}
### plan 실패
{{if .FailureByAI}}
{{ailabel}}
{{end}}
{{line .Failure}}
{{else}}
### 변경 요약

추가 {{.Summary.Add}} / 변경 {{.Summary.Change}} / 삭제 {{.Summary.Destroy}} / 교체 {{.Summary.Replace}}
{{if .Summary.Targets}}
| 리소스 | 작업 | 변경 attribute |
| --- | --- | --- |
{{range .Summary.Targets -}}
| {{code .Address}} | {{text .Action}} | {{attrs .Attrs}} |
{{end -}}
{{end}}
{{- if .Summary.Truncated}}
> 대상이 많아 일부만 표시했습니다.
{{end}}
{{- if .Risks}}
### 위험 소견

{{ailabel}}

{{range .Risks -}}
- {{line .}}
{{end -}}
{{end}}
{{- end}}
{{- end}}`))

// CodeReview renders the code review comment, starting with its marker.
func CodeReview(v CodeReviewView) string {
	return execute(codeTmpl, v)
}

// Plan renders the plan comment, starting with its marker.
func Plan(v PlanView) string {
	return execute(planTmpl, v)
}

func execute(t *template.Template, data any) string {
	var b strings.Builder
	if err := t.Execute(&b, data); err != nil {
		// Templates and data are static, so this signals a programming error.
		panic(fmt.Errorf("render %s: %w", t.Name(), err))
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func shortSHA(sha string) string {
	if len(sha) > shortSHAN {
		return sha[:shortSHAN]
	}
	return sha
}

func passedLabel(passed bool) string {
	if passed {
		return "통과"
	}
	return "실패"
}

func basisNote(basis string) string {
	switch basis {
	case "lock":
		return "_근거: lock 파일의 provider 버전에 맞는 Terraform 공식 문서._"
	case "constraint":
		return "_근거: 버전 제약 조건에 맞춘 Terraform 공식 문서. 실제 사용 버전과 다를 수 있습니다._"
	default:
		return "_공식 문서 근거를 사용하지 않았습니다._"
	}
}

var lineBreaks = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ")

// oneLine collapses line breaks into single spaces.
func oneLine(s string) string {
	return lineBreaks.Replace(s)
}

const trustedDocHost = "registry.terraform.io"

var docPath = regexp.MustCompile(`^/[A-Za-z0-9/._-]+$`)

// docRefCell renders DocRef as a plain link only when it passes the registry
// allowlist; anything else may come from the LLM and is neutralized.
func docRefCell(s string) string {
	if isRegistryURL(s) {
		return plainCell(s)
	}
	return safeCell(s)
}

// isRegistryURL allows only https://registry.terraform.io/<simple path>: no
// user info, port, query, fragment or escapes.
func isRegistryURL(s string) bool {
	if strings.ContainsAny(s, "?#") {
		return false
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	return u.Scheme == "https" && u.Host == trustedDocHost && u.User == nil &&
		u.Opaque == "" && docPath.MatchString(u.EscapedPath()) && u.Path == u.EscapedPath()
}

// safeCell neutralizes untrusted text, then makes it safe for a table cell.
func safeCell(s string) string {
	return plainCell(neutralize(s))
}

// safeLine neutralizes untrusted text and collapses it to one line.
func safeLine(s string) string {
	return oneLine(neutralize(s))
}

// plainCell makes s safe for a markdown table cell.
func plainCell(s string) string {
	return strings.ReplaceAll(oneLine(s), "|", `\|`)
}

// codeSpan wraps s in a table-safe code span. The backtick fence is longer
// than any backtick run inside s, with padding spaces where CommonMark would
// otherwise swallow a leading or trailing backtick or space.
func codeSpan(s string) string {
	s = plainCell(s)
	if s == "" {
		return ""
	}
	fence := strings.Repeat("`", longestBacktickRun(s)+1)
	if needsPadding(s) {
		return fence + " " + s + " " + fence
	}
	return fence + s + fence
}

func needsPadding(s string) bool {
	return strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") ||
		(strings.HasPrefix(s, " ") && strings.HasSuffix(s, " "))
}

func longestBacktickRun(s string) int {
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return longest
}

// attrList renders names (attributes, addresses) as comma-separated code spans.
func attrList(names []string) string {
	spans := make([]string, len(names))
	for i, n := range names {
		spans[i] = codeSpan(n)
	}
	return strings.Join(spans, ", ")
}
