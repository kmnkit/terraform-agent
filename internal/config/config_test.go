package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validPath = "testdata/valid.json"

// mutate returns the path of a copy of the valid fixture with the first
// occurrence of old replaced by repl.
func mutate(t *testing.T, old, repl string) string {
	t.Helper()
	raw, err := os.ReadFile(validPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, old) {
		t.Fatalf("fixture lacks %q", old)
	}
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(strings.Replace(s, old, repl, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_Valid(t *testing.T) {
	c, err := Load(validPath)
	if err != nil {
		t.Fatal(err)
	}
	if c.AllowedGitlabProjectID != 4242 || c.TfeOrganization != "acme" || c.Model != "gemini-3.8-flash" ||
		c.VertexProject != "acme-prod" || c.VertexLocation != "asia-northeast3" {
		t.Errorf("unexpected config: %+v", c)
	}
	if len(c.Workspaces) != 2 || c.McpToolAllowlist["searchProviderDocs"] != "search_provider_docs" {
		t.Errorf("unexpected lists: %+v", c)
	}
	if c.Limits.DiffBytes != 100000 || c.Limits.LLMOutputBytes != 8000 {
		t.Errorf("limits: %+v", c.Limits)
	}
	if c.Waits.Initial != 10*time.Minute || c.Waits.Total != 16*time.Minute {
		t.Errorf("waits: %+v", c.Waits)
	}
	if c.TokenFiles.TFE != "/secrets/tfe/token" || c.TokenFiles.GitLab != "/secrets/gitlab/token" {
		t.Errorf("tokenFiles: %+v", c.TokenFiles)
	}
}

func TestLoad_DefaultsGitlabBaseURL(t *testing.T) {
	c, err := Load(mutate(t, `"gitlabBaseURL": "https://gitlab.com",`, ""))
	if err != nil {
		t.Fatal(err)
	}
	if c.GitlabBaseURL != "https://gitlab.com" {
		t.Errorf("got %q", c.GitlabBaseURL)
	}
}

func TestLoad_RejectsUnknownField(t *testing.T) {
	_, err := Load(mutate(t, `"model":`, `"bogusKey": 1, "model":`))
	if err == nil || !strings.Contains(err.Error(), "bogusKey") {
		t.Fatalf("want error naming bogusKey, got %v", err)
	}
}

func TestLoad_RejectsUnknownNestedField(t *testing.T) {
	_, err := Load(mutate(t, `"diffBytes"`, `"nestedBogus": 1, "diffBytes"`))
	if err == nil || !strings.Contains(err.Error(), "nestedBogus") {
		t.Fatalf("want error naming nestedBogus, got %v", err)
	}
	_, err = Load(mutate(t, `"initial"`, `"waitBogus": "1s", "initial"`))
	if err == nil || !strings.Contains(err.Error(), "waitBogus") {
		t.Fatalf("want error naming waitBogus, got %v", err)
	}
}

func TestLoad_RequiresAllowedProjectID(t *testing.T) {
	for _, repl := range []string{`"allowedGitlabProjectID": 0`, `"allowedGitlabProjectID": -1`} {
		_, err := Load(mutate(t, `"allowedGitlabProjectID": 4242`, repl))
		if err == nil || !strings.Contains(err.Error(), "allowedGitlabProjectID") {
			t.Errorf("%s: got %v", repl, err)
		}
	}
	_, err := Load(mutate(t, `"allowedGitlabProjectID": 4242,`, ""))
	if err == nil || !strings.Contains(err.Error(), "allowedGitlabProjectID") {
		t.Errorf("missing: got %v", err)
	}
}

func TestLoad_RejectsNonPositiveLimits(t *testing.T) {
	cases := map[string]string{
		"diffBytes":        `"diffBytes": 100000`,
		"docsBytes":        `"docsBytes": 50000`,
		"planSummaryBytes": `"planSummaryBytes": 20000`,
		"llmOutputBytes":   `"llmOutputBytes": 8000`,
	}
	for key, old := range cases {
		for _, v := range []string{"0", "-5"} {
			_, err := Load(mutate(t, old, `"`+key+`": `+v))
			if err == nil || !strings.Contains(err.Error(), key) {
				t.Errorf("%s=%s: got %v", key, v, err)
			}
		}
	}
}

func TestLoad_RejectsInvalidWaits(t *testing.T) {
	for _, repl := range []string{`"initial": "0s"`, `"initial": "-1m"`, `"initial": "soon"`, `"initial": 30`} {
		if _, err := Load(mutate(t, `"initial": "10m"`, repl)); err == nil {
			t.Errorf("%s: want error", repl)
		}
	}
	if _, err := Load(mutate(t, `"total": "16m"`, `"total": "0s"`)); err == nil {
		t.Error("total 0s: want error")
	}
}

// Workspaces are waited sequentially inside ProcessDeadline - WaitReserve
// (40m - 8m = 32m); the fixture has 2 workspaces.
func TestLoad_TotalWaitAcrossWorkspacesWithinBudget(t *testing.T) {
	cases := []struct {
		total   string
		wantErr bool
	}{
		{"16m", false},     // 2 x 16m = 32m: exactly the budget
		{"16m500ms", true}, // 2 x 16m0.5s = 32m1s
		{"20m", true},      // 2 x 20m = 40m: no reserve left
		{"2562047h", true}, // huge value must not overflow into a pass
	}
	for _, tc := range cases {
		_, err := Load(mutate(t, `"total": "16m"`, `"total": "`+tc.total+`"`))
		if tc.wantErr && (err == nil || !strings.Contains(err.Error(), "waits.total")) {
			t.Errorf("total %s: want waits.total error, got %v", tc.total, err)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("total %s: unexpected error %v", tc.total, err)
		}
	}
}

// The find phase is bounded by Initial, so Initial > Total would let one
// workspace wait longer than Total.
func TestLoad_RejectsInitialAboveTotal(t *testing.T) {
	if _, err := Load(mutate(t, `"initial": "10m"`, `"initial": "16m1s"`)); err == nil ||
		!strings.Contains(err.Error(), "waits.initial") {
		t.Fatalf("want waits.initial error, got %v", err)
	}
	if _, err := Load(mutate(t, `"initial": "10m"`, `"initial": "16m"`)); err != nil {
		t.Fatalf("initial == total: unexpected error %v", err)
	}
}

func TestLoad_RejectsBadStrings(t *testing.T) {
	cases := []struct{ name, old, repl, want string }{
		{"tfeAddress", `"tfeAddress": "https://app.terraform.io"`, `"tfeAddress": "https://tfe.example.com"`, "tfeAddress"},
		{"org", `"tfeOrganization": "acme"`, `"tfeOrganization": ""`, "tfeOrganization"},
		{"workspaces", `"workspaces": ["prod-network", "prod-app"]`, `"workspaces": []`, "workspaces"},
		{"workspaceEmpty", `"prod-app"`, `""`, "workspaces"},
		{"mcpBinary", `"mcpBinaryPath": "/usr/local/bin/terraform-mcp-server"`, `"mcpBinaryPath": ""`, "mcpBinaryPath"},
		{"allowlist", `{"searchProviderDocs": "search_provider_docs"}`, `{}`, "mcpToolAllowlist"},
		{"allowlistValue", `"search_provider_docs"`, `""`, "mcpToolAllowlist"},
		{"model", `"model": "gemini-3.8-flash"`, `"model": ""`, "model"},
		{"modelLatest", `"gemini-3.8-flash"`, `"gemini-flash-latest"`, "model"},
		{"vertexProject", `"vertexProject": "acme-prod"`, `"vertexProject": ""`, "vertexProject"},
		{"vertexLocationEmpty", `"vertexLocation": "asia-northeast3"`, `"vertexLocation": ""`, "vertexLocation"},
		{"vertexLocationPattern", `"asia-northeast3"`, `"https://evil.example/"`, "vertexLocation"},
		{"tfeToken", `"tfe": "/secrets/tfe/token"`, `"tfe": ""`, "tokenFiles"},
		{"gitlabToken", `"gitlab": "/secrets/gitlab/token"`, `"gitlab": ""`, "tokenFiles"},
	}
	for _, c := range cases {
		_, err := Load(mutate(t, c.old, c.repl))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want mention of %q", c.name, err, c.want)
		}
	}
}

func TestLoad_FileAndSyntaxErrors(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("missing file: want error")
	}
	p := filepath.Join(t.TempDir(), "bad.json")
	for _, body := range []string{"{", `{} {}`} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Errorf("%q: want error", body)
		}
	}
}
