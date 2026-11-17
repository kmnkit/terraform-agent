package input

import (
	"strings"
	"testing"
)

const goodSHA = "0123456789abcdef0123456789abcdef01234567"

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

var okEnv = map[string]string{"CLOUD_RUN_TASK_COUNT": "1", "CLOUD_RUN_TASK_INDEX": "0"}

func goodArgs() []string {
	return []string{"--mr-iid", "42", "--sha", goodSHA, "--pipeline-id", "7"}
}

func TestParse_Valid(t *testing.T) {
	got, err := Parse(goodArgs(), envOf(okEnv))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Input{MRIID: 42, SHA: goodSHA, PipelineID: 7}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestParse_OnlyReadsTaskEnv(t *testing.T) {
	var keys []string
	getenv := func(k string) string { keys = append(keys, k); return okEnv[k] }
	if _, err := Parse(goodArgs(), getenv); err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if k != "CLOUD_RUN_TASK_COUNT" && k != "CLOUD_RUN_TASK_INDEX" {
			t.Fatalf("unexpected env key requested: %q", k)
		}
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 env reads, got %v", keys)
	}
}

func TestParse_RejectsUnknownFlag(t *testing.T) {
	args := append(goodArgs(), "--project-id", "1")
	assertErr(t, args, okEnv, "project-id")
}

func TestParse_RejectsPositionalArgs(t *testing.T) {
	assertErr(t, append(goodArgs(), "extra"), okEnv, "positional")
}

func TestParse_RejectsMissingFlag(t *testing.T) {
	cases := map[string][]string{
		"mr-iid":      {"--sha", goodSHA, "--pipeline-id", "7"},
		"sha":         {"--mr-iid", "42", "--pipeline-id", "7"},
		"pipeline-id": {"--mr-iid", "42", "--sha", goodSHA},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) { assertErr(t, args, okEnv, name) })
	}
}

func TestParse_RejectsNonNumeric(t *testing.T) {
	cases := [][]string{
		{"--mr-iid", "abc", "--sha", goodSHA, "--pipeline-id", "7"},
		{"--mr-iid", "0", "--sha", goodSHA, "--pipeline-id", "7"},
		{"--mr-iid", "-3", "--sha", goodSHA, "--pipeline-id", "7"},
		{"--mr-iid", "42", "--sha", goodSHA, "--pipeline-id", "x"},
		{"--mr-iid", "42", "--sha", goodSHA, "--pipeline-id", "0"},
		{"--mr-iid", "010", "--sha", goodSHA, "--pipeline-id", "7"},
		{"--mr-iid", "0x10", "--sha", goodSHA, "--pipeline-id", "7"},
		{"--mr-iid", "1_0", "--sha", goodSHA, "--pipeline-id", "7"},
		{"--mr-iid", "42", "--sha", goodSHA, "--pipeline-id", "010"},
		{"--mr-iid", "42", "--sha", goodSHA, "--pipeline-id", "0x10"},
		{"--mr-iid", "42", "--sha", goodSHA, "--pipeline-id", "1_0"},
		{"--mr-iid", "99999999999999999999", "--sha", goodSHA, "--pipeline-id", "7"},
		{"--mr-iid", "+5", "--sha", goodSHA, "--pipeline-id", "7"},
	}
	for _, args := range cases {
		assertErr(t, args, okEnv, "")
	}
}

func TestParse_RejectsSHAFormat(t *testing.T) {
	bad := []string{
		"abc123",
		strings.ToUpper(goodSHA),
		goodSHA + "0",
		"zz" + goodSHA[2:],
		"HEAD",
	}
	for _, sha := range bad {
		args := []string{"--mr-iid", "42", "--sha", sha, "--pipeline-id", "7"}
		assertErr(t, args, okEnv, "sha")
	}
}

func TestParse_RejectsTaskCountNotOne(t *testing.T) {
	for _, v := range []string{"", "0", "2", "01", "abc"} {
		env := map[string]string{"CLOUD_RUN_TASK_COUNT": v, "CLOUD_RUN_TASK_INDEX": "0"}
		assertErr(t, goodArgs(), env, "CLOUD_RUN_TASK_COUNT")
	}
}

func TestParse_RejectsTaskIndexNotZero(t *testing.T) {
	for _, v := range []string{"", "1", "00", "abc"} {
		env := map[string]string{"CLOUD_RUN_TASK_COUNT": "1", "CLOUD_RUN_TASK_INDEX": v}
		assertErr(t, goodArgs(), env, "CLOUD_RUN_TASK_INDEX")
	}
}

func assertErr(t *testing.T, args []string, env map[string]string, wantSub string) {
	t.Helper()
	_, err := Parse(args, envOf(env))
	if err == nil {
		t.Fatalf("expected error for args=%v env=%v", args, env)
	}
	if !strings.Contains(err.Error(), wantSub) {
		t.Fatalf("error %q does not contain %q", err, wantSub)
	}
}
