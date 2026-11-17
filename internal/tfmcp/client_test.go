package tfmcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/terraform-agent/internal/config"
)

var fakeBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fakeserver")
	if err != nil {
		panic(err)
	}
	fakeBinary = filepath.Join(dir, "fakeserver")
	out, err := exec.Command("go", "build", "-o", fakeBinary, "./testdata/fakeserver").CombinedOutput()
	if err != nil {
		os.RemoveAll(dir)
		panic(string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

const testToken = "tfe-token-sentinel"

func testConfig(extra map[string]string) config.Config {
	allow := map[string]string{
		"echoEnv": "echo_env", "noisy": "noisy", "limited": "rate_limited",
		"fail": "fail", "big": "big",
	}
	for k, v := range extra {
		allow[k] = v
	}
	return config.Config{
		McpBinaryPath:    fakeBinary,
		McpToolAllowlist: allow,
		TfeAddress:       config.TfeAddress,
	}
}

func writeToken(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tfe-token")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func startClient(t *testing.T, cfg config.Config) *Client {
	t.Helper()
	c, err := Start(context.Background(), cfg, writeToken(t, " "+testToken+"\n"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

func childEnv(t *testing.T, c *Client) map[string]string {
	t.Helper()
	out, err := c.Call(context.Background(), "echoEnv", nil)
	if err != nil {
		t.Fatal(err)
	}
	var env []string
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	return got
}

func TestStart_ChildEnvIsMinimal(t *testing.T) {
	t.Setenv("PARENT_SENTINEL", "leak-me")
	c := startClient(t, testConfig(nil))
	got := childEnv(t, c)
	if got["TFE_ADDRESS"] != "https://app.terraform.io" {
		t.Errorf("TFE_ADDRESS = %q", got["TFE_ADDRESS"])
	}
	if got["TFE_TOKEN"] != testToken {
		t.Errorf("TFE_TOKEN is not the trimmed token file content")
	}
	if got["HOME"] == "" || got["HOME"] == os.Getenv("HOME") || got["HOME"] != c.home {
		t.Errorf("HOME = %q, want fresh temp dir %q", got["HOME"], c.home)
	}
	for k := range got {
		switch k {
		case "TFE_ADDRESS", "TFE_TOKEN", "HOME":
		default:
			t.Errorf("unexpected child env var %q", k)
		}
	}
}

func TestStart_EmptyOrMissingTokenIsError(t *testing.T) {
	if _, err := Start(context.Background(), testConfig(nil), writeToken(t, "  \n")); err == nil {
		t.Fatal("want error for empty token")
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := Start(context.Background(), testConfig(nil), missing); err == nil {
		t.Fatal("want error for missing token file")
	}
}

func TestStart_BadBinaryFails(t *testing.T) {
	cfg := testConfig(nil)
	cfg.McpBinaryPath = filepath.Join(t.TempDir(), "nope")
	if _, err := Start(context.Background(), cfg, writeToken(t, "t")); err == nil {
		t.Fatal("want start error")
	}
}

func TestStart_ChildStderrNotForwarded(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()
	c := startClient(t, testConfig(nil))
	if _, err := c.Call(context.Background(), "noisy", nil); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	w.Close()
	buf := make([]byte, 1<<16)
	n, _ := r.Read(buf)
	if strings.Contains(string(buf[:n]), "NOISE-FROM-CHILD-STDERR") {
		t.Error("child stderr reached parent stderr")
	}
}

func TestClose_RemovesHome(t *testing.T) {
	c := startClient(t, testConfig(nil))
	home := c.home
	if _, err := os.Stat(home); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("home still exists: %v", err)
	}
}

func TestVerifyTools(t *testing.T) {
	c := startClient(t, testConfig(nil))
	if err := c.VerifyTools(context.Background()); err != nil {
		t.Fatalf("all present: %v", err)
	}
}

func TestVerifyTools_FailsWhenAllowlistMissing(t *testing.T) {
	c := startClient(t, testConfig(map[string]string{"ghost": "no_such_tool"}))
	err := c.VerifyTools(context.Background())
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "ghost") || !strings.Contains(err.Error(), "no_such_tool") {
		t.Errorf("error should name logical and actual: %v", err)
	}
}

func TestCall_UnknownLogicalName(t *testing.T) {
	c := startClient(t, testConfig(nil))
	if _, err := c.Call(context.Background(), "nope", nil); err == nil {
		t.Fatal("want error")
	}
}

func TestCall_RetriesOnRateLimitText(t *testing.T) {
	for _, phrase := range []string{
		"rate limit exceeded: too many requests from this session",
		"Rate limit exceeded: too many requests globally",
	} {
		c := startClient(t, testConfig(nil))
		var sleeps []time.Duration
		c.sleep = func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
		out, err := c.Call(context.Background(), "limited", map[string]any{"failures": 3, "phrase": phrase})
		if err != nil || out != "finally" {
			t.Fatalf("out=%q err=%v", out, err)
		}
		if len(sleeps) != 3 || sleeps[1] != 2*sleeps[0] || sleeps[2] != 2*sleeps[1] {
			t.Errorf("backoff not exponential: %v", sleeps)
		}
	}
}

func TestCall_RateLimitGivesUpAfterFourAttempts(t *testing.T) {
	c := startClient(t, testConfig(nil))
	n := 0
	c.sleep = func(context.Context, time.Duration) error { n++; return nil }
	_, err := c.Call(context.Background(), "limited",
		map[string]any{"failures": 99, "phrase": "rate limit exceeded: too many requests globally"})
	if err == nil {
		t.Fatal("want error")
	}
	if n != 3 {
		t.Errorf("sleeps = %d, want 3 (4 attempts)", n)
	}
}

func TestCall_RateLimitHonoursContext(t *testing.T) {
	c := startClient(t, testConfig(nil))
	ctx, cancel := context.WithCancel(context.Background())
	c.sleep = func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }
	_, err := c.Call(ctx, "limited",
		map[string]any{"failures": 99, "phrase": "rate limit exceeded: too many requests globally"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestCall_DoesNotRetryOtherErrors(t *testing.T) {
	c := startClient(t, testConfig(nil))
	n := 0
	c.sleep = func(context.Context, time.Duration) error { n++; return nil }
	if _, err := c.Call(context.Background(), "fail", nil); err == nil {
		t.Fatal("want error")
	}
	if n != 0 {
		t.Errorf("slept %d times, want 0", n)
	}
}

func TestCall_ErrorDoesNotLeakPayload(t *testing.T) {
	c := startClient(t, testConfig(nil))
	_, err := c.Call(context.Background(), "fail", map[string]any{"secret": "ARG-SECRET"})
	if err == nil {
		t.Fatal("want error")
	}
	for _, bad := range []string{"RESPONSE-PAYLOAD", "ARG-SECRET"} {
		if strings.Contains(err.Error(), bad) {
			t.Errorf("error leaks %q: %v", bad, err)
		}
	}
	if !strings.Contains(err.Error(), "fail") {
		t.Errorf("error should name the tool: %v", err)
	}
	_, err = c.Call(context.Background(), "limited",
		map[string]any{"failures": 99, "phrase": "rate limit exceeded: PAYLOAD-X"})
	if err == nil || strings.Contains(err.Error(), "PAYLOAD-X") {
		t.Errorf("rate limit error leaks payload: %v", err)
	}
}

func TestCall_LargeResponse(t *testing.T) {
	c := startClient(t, testConfig(nil))
	out, err := c.Call(context.Background(), "big", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2<<20 {
		t.Errorf("len = %d", len(out))
	}
}
