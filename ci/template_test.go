package ci_test

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/terraform-agent/internal/staticcheck"
)

const (
	templatePath = "tf-review.gitlab-ci.yml"
	triggerJob   = "tf-review-trigger"
	mrOnlyRule   = `$CI_PIPELINE_SOURCE == "merge_request_event"`
)

var forbiddenFlags = []string{"--update-env-vars", "--set-env-vars", "--tasks", "--task-timeout"}

func loadTemplate(t *testing.T) (string, map[string]map[string]any) {
	t.Helper()
	raw, err := os.ReadFile(templatePath)
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	var doc map[string]map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse template: %v", err)
	}
	return string(raw), doc
}

func scriptLines(t *testing.T, job map[string]any) []string {
	t.Helper()
	items, ok := job["script"].([]any)
	if !ok {
		t.Fatalf("script is %T, want list", job["script"])
	}
	lines := make([]string, 0, len(items))
	for _, it := range items {
		lines = append(lines, it.(string))
	}
	return lines
}

func hasMRRule(job map[string]any) bool {
	rules, _ := job["rules"].([]any)
	for _, r := range rules {
		if m, ok := r.(map[string]any); ok && m["if"] == mrOnlyRule {
			return true
		}
	}
	return false
}

func TestTemplate_StaticcheckJobProducesArtifact(t *testing.T) {
	raw, doc := loadTemplate(t)
	job, ok := doc[staticcheck.JobName]
	if !ok {
		t.Fatalf("job %q missing", staticcheck.JobName)
	}
	if !strings.Contains(raw, staticcheck.ArtifactName) {
		t.Errorf("template does not mention %q", staticcheck.ArtifactName)
	}
	art, _ := job["artifacts"].(map[string]any)
	paths, _ := art["paths"].([]any)
	if len(paths) != 1 || paths[0] != staticcheck.ArtifactName {
		t.Errorf("artifacts.paths = %v, want [%s]", paths, staticcheck.ArtifactName)
	}
	if art["when"] != "always" {
		t.Errorf("artifacts.when = %v, want always", art["when"])
	}
	if art["expire_in"] != "1 day" {
		t.Errorf("artifacts.expire_in = %v, want 1 day", art["expire_in"])
	}
	if !hasMRRule(job) {
		t.Error("static check job lacks the MR pipeline rule")
	}
	if job["allow_failure"] != true {
		t.Error("static check job must set allow_failure: true")
	}
}

func TestTemplate_StaticcheckRunsAllFourTools(t *testing.T) {
	_, doc := loadTemplate(t)
	script := strings.Join(scriptLines(t, doc[staticcheck.JobName]), "\n")
	for _, want := range []string{
		"terraform fmt -check -recursive",
		"terraform init -backend=false",
		"terraform validate",
		"tflint --recursive",
		"trivy config",
		"sha256sum -c",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("static check script lacks %q", want)
		}
	}
}

func TestTemplate_TriggerJobRulesAndNeeds(t *testing.T) {
	_, doc := loadTemplate(t)
	job, ok := doc[triggerJob]
	if !ok {
		t.Fatalf("job %q missing", triggerJob)
	}
	if !hasMRRule(job) {
		t.Error("trigger job lacks the MR pipeline rule")
	}
	if job["allow_failure"] != true {
		t.Error("trigger job must set allow_failure: true")
	}
	needs, _ := job["needs"].([]any)
	if len(needs) != 1 || needs[0] != staticcheck.JobName {
		t.Errorf("needs = %v, want [%s]", needs, staticcheck.JobName)
	}
}

func TestTemplate_TriggerArgsAreExactlyThreeFlags(t *testing.T) {
	_, doc := loadTemplate(t)
	var execLines []string
	for _, l := range scriptLines(t, doc[triggerJob]) {
		if strings.Contains(l, "gcloud run jobs execute") {
			execLines = append(execLines, l)
		}
	}
	if len(execLines) != 1 {
		t.Fatalf("got %d execute lines, want 1", len(execLines))
	}
	m := regexp.MustCompile(`--args="([^"]*)"`).FindStringSubmatch(execLines[0])
	if m == nil {
		t.Fatalf("no --args in %q", execLines[0])
	}
	got := strings.Split(m[1], ",")
	want := []string{
		"--mr-iid=${CI_MERGE_REQUEST_IID}",
		"--sha=${TF_REVIEW_SHA}",
		"--pipeline-id=${CI_PIPELINE_ID}",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("args = %v, want %v", got, want)
	}
}

// GitLab variable expansion has no shell defaults (${A:-B}), so the SHA
// fallback must be computed by the script's shell before the execute line.
func TestTemplate_TriggerSHAComputedInScript(t *testing.T) {
	_, doc := loadTemplate(t)
	job := doc[triggerJob]
	if vars, ok := job["variables"].(map[string]any); ok {
		for k, v := range vars {
			if strings.Contains(fmt.Sprint(v), ":-") {
				t.Errorf("variables.%s uses a shell default, which GitLab does not expand: %v", k, v)
			}
		}
	}
	const assign = `TF_REVIEW_SHA="${CI_MERGE_REQUEST_SOURCE_BRANCH_SHA:-$CI_COMMIT_SHA}"`
	assignAt, execAt := -1, -1
	for i, l := range scriptLines(t, job) {
		if strings.TrimSpace(l) == assign {
			assignAt = i
		}
		if strings.Contains(l, "gcloud run jobs execute") {
			execAt = i
		}
	}
	if assignAt < 0 || execAt < 0 || assignAt > execAt {
		t.Errorf("script must assign %s before the execute line (assign %d, execute %d)", assign, assignAt, execAt)
	}
}

func TestTemplate_NoOverrideFlags(t *testing.T) {
	raw, _ := loadTemplate(t)
	for _, flag := range forbiddenFlags {
		if strings.Contains(raw, flag) {
			t.Errorf("template contains forbidden override flag %q", flag)
		}
	}
}

func TestTemplate_EveryImageIsDigestPinned(t *testing.T) {
	_, doc := loadTemplate(t)
	images := 0
	for name, job := range doc {
		var ref string
		switch v := job["image"].(type) {
		case string:
			ref = v
		case map[string]any:
			ref, _ = v["name"].(string)
		default:
			continue
		}
		images++
		if !strings.Contains(ref, "@sha256:") {
			t.Errorf("job %q image %q is not digest-pinned", name, ref)
		}
	}
	if images != 2 {
		t.Errorf("found %d images, want 2", images)
	}
}
