package tfmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
		"fail": "fail", "big": "big", "hang": "hang",
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

func homeDirCount(t *testing.T) int {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(os.TempDir(), "tfmcp-home*"))
	if err != nil {
		t.Fatal(err)
	}
	return len(m)
}

func TestStart_MissingBinaryPropagatesCauseAndRemovesHome(t *testing.T) {
	cfg := testConfig(nil)
	cfg.McpBinaryPath = filepath.Join(t.TempDir(), "nope")
	before := homeDirCount(t)
	_, err := Start(context.Background(), cfg, writeToken(t, "t"))
	var execErr *exec.Error
	var pathErr *fs.PathError
	if !errors.As(err, &execErr) && !errors.As(err, &pathErr) {
		t.Fatalf("want *exec.Error or *fs.PathError, got %v", err)
	}
	if after := homeDirCount(t); after != before {
		t.Errorf("tfmcp-home dirs before=%d after=%d", before, after)
	}
}

func TestStart_DeadlineExceededPropagatesAndRemovesHome(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	before := homeDirCount(t)
	_, err := Start(ctx, testConfig(nil), writeToken(t, "t"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if after := homeDirCount(t); after != before {
		t.Errorf("tfmcp-home dirs before=%d after=%d", before, after)
	}
}

func TestVerifyTools_ContextErrorPropagates(t *testing.T) {
	c := startClient(t, testConfig(nil))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.VerifyTools(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want Canceled", err)
	}
}

func TestClose_Idempotent(t *testing.T) {
	c := startClient(t, testConfig(nil))
	first := c.Close()
	if second := c.Close(); second != first {
		t.Errorf("second Close = %v, first = %v", second, first)
	}
}

// Positive control: proves the fake server really writes noise to stderr, so
// the "not forwarded" assertion above can fail.
func TestFakeServer_NoisyWritesToStderr(t *testing.T) {
	var buf bytes.Buffer
	cmd := exec.Command(fakeBinary, "--tools=noisy")
	cmd.Stderr = &buf
	ctx := context.Background()
	impl := &mcp.Implementation{Name: "ctl", Version: "0"}
	s, err := mcp.NewClient(impl, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "noisy"}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if !strings.Contains(buf.String(), "NOISE-FROM-CHILD-STDERR") {
		t.Errorf("noise missing from captured stderr: %q", buf.String())
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
		if len(sleeps) != 3 || sleeps[0] != time.Second || sleeps[1] != 2*sleeps[0] || sleeps[2] != 2*sleeps[1] {
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

func TestCall_PerCallTimeout(t *testing.T) {
	if callTimeout != 60*time.Second {
		t.Fatalf("callTimeout = %v, want 60s", callTimeout)
	}
	c := startClient(t, testConfig(nil))
	c.callTimeout = 200 * time.Millisecond
	start := time.Now()
	_, err := c.Call(context.Background(), "hang", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("call took %v, want it bounded by the per-call timeout", elapsed)
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
