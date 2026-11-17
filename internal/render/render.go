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
const aiLabel = "_AIが作成した所見です。参考情報として扱ってください。_"

var codeTmpl = template.Must(template.New(codeKind).Funcs(funcs).Parse(`{{marker "code" "-"}}
## Terraform コードレビュー

基準コミット: {{code (short .SHA)}}
{{- if .AnalysisFailed}}

> 分析失敗: 自動分析を完了できませんでした。静的チェックの結果のみ表示します。
{{- end}}
{{- if .Truncated}}

> 変更が大きいため、一部のみレビューしました。
{{- end}}

### 静的チェック
{{if .Checks}}
| ツール | 結果 | 概要 |
| --- | --- | --- |
{{range .Checks -}}
| {{text .Tool}} | {{passed .Passed}} | {{text .Summary}} |
{{end -}}
{{else}}
静的チェックの結果はありません
{{end}}
### レビュー所見
{{if .Findings}}
{{ailabel}}

| 重大度 | リソース | 内容 | ドキュメント |
| --- | --- | --- | --- |
{{range .Findings -}}
| {{text .Severity}} | {{code .Resource}} | {{text .Message}} | {{docref .DocRef}} |
{{end -}}
{{else if .AnalysisFailed}}
所見を生成できませんでした。
{{else if .NotReviewed}}
ドキュメントの根拠があるリソースがないため、AI レビューは行いませんでした。
{{else}}
指摘事項はありません
{{end}}
{{- if .Skipped}}
ドキュメントの根拠がないためレビュー対象外: {{attrs .Skipped}}
{{end}}
{{- if .Removed}}
削除されたリソース (ドキュメント照会の対象外): {{attrs .Removed}}
{{end}}
{{if not .AnalysisFailed}}{{basis .VersionBasis}}{{end}}
`))

var planTmpl = template.Must(template.New(planKind).Funcs(funcs).Parse(`{{marker "plan" .Workspace}}
## Terraform plan 分析

workspace: {{code .Workspace}}
基準コミット: {{code (short .SHA)}}
{{- if and .RunURL (not (is .Status "unverified")) (not (is .Status "error"))}}
実行: {{.RunURL}}
{{- end}}
{{if is .Status "unverified"}}
> このコミットの plan を確認できませんでした。リンクされた HCP Terraform の run がこのコミットの run であることを確認できません。
{{- else if is .Status "error"}}
> 分析失敗: このコミットの plan を分析できませんでした。HCP Terraform の実行画面で確認してください。
{{- else if is .Status "noplan"}}
> このコミットに対応する plan が見つかりませんでした。
{{- else if is .Status "timeout"}}
> 待機時間内に plan が完了しませんでした。最後に確認した状態: {{if .Queued}}待機中 (queued){{else}}実行中 (running){{end}}
{{- else}}
{{- if is .Status "analysisFailed"}}
> 分析失敗: plan の概要のみ表示します。リスク分析は提供されません。
{{end}}
{{- if is .Status "failed"}}
### plan 失敗
{{if .FailureByAI}}
{{ailabel}}
{{end}}
{{line .Failure}}
{{else}}
### 変更概要

追加 {{.Summary.Add}} / 変更 {{.Summary.Change}} / 削除 {{.Summary.Destroy}} / 置換 {{.Summary.Replace}}
{{if .Summary.Targets}}
| リソース | 操作 | 変更された attribute |
| --- | --- | --- |
{{range .Summary.Targets -}}
| {{code .Address}} | {{text .Action}} | {{attrs .Attrs}} |
{{end -}}
{{end}}
{{- if .Summary.Truncated}}
> 対象が多いため、一部のみ表示しています。
{{end}}
{{- if .Risks}}
### リスク所見

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
		return "成功"
	}
	return "失敗"
}

func basisNote(basis string) string {
	switch basis {
	case "lock":
		return "_根拠: lock ファイルの provider バージョンに対応する Terraform 公式ドキュメント。_"
	case "constraint":
		return "_根拠: バージョン制約に基づく Terraform 公式ドキュメント。実際に使用されるバージョンと異なる場合があります。_"
	default:
		return "_公式ドキュメントの根拠は使用していません。_"
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
