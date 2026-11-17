// Package tfmcp wraps the terraform-mcp-server child process behind a small
// allowlisted Call interface.
package tfmcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/terraform-agent/internal/config"
)

const (
	maxAttempts    = 4
	baseBackoff    = time.Second
	rateLimitPhase = "rate limit exceeded"
)

// Caller invokes an allowlisted MCP tool by its logical name.
type Caller interface {
	Call(ctx context.Context, logicalName string, args map[string]any) (string, error)
}

// Client is a running terraform-mcp-server session.
type Client struct {
	session   *mcp.ClientSession
	allowlist map[string]string
	home      string
	sleep     func(ctx context.Context, d time.Duration) error
}

// Start launches cfg.McpBinaryPath with only the allowlisted tools enabled and
// a minimal explicit environment (TFE_ADDRESS, TFE_TOKEN, an empty temp HOME).
// The child's stderr is discarded.
func Start(ctx context.Context, cfg config.Config, tfeTokenFile string) (*Client, error) {
	raw, err := os.ReadFile(tfeTokenFile)
	if err != nil {
		return nil, fmt.Errorf("tfmcp: read token file: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return nil, errors.New("tfmcp: token file is empty")
	}
	home, err := os.MkdirTemp("", "tfmcp-home")
	if err != nil {
		return nil, fmt.Errorf("tfmcp: create temp HOME: %w", err)
	}
	cmd := exec.Command(cfg.McpBinaryPath, "--tools="+toolArg(cfg.McpToolAllowlist), "stdio")
	cmd.Env = []string{"TFE_ADDRESS=" + cfg.TfeAddress, "TFE_TOKEN=" + token, "HOME=" + home}
	cmd.Stderr = io.Discard
	impl := &mcp.Implementation{Name: "terraform-agent", Version: "0"}
	session, err := mcp.NewClient(impl, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		_ = os.RemoveAll(home)
		return nil, errors.New("tfmcp: start MCP server failed")
	}
	return &Client{
		session:   session,
		allowlist: cfg.McpToolAllowlist,
		home:      home,
		sleep:     sleepCtx,
	}, nil
}

func toolArg(allow map[string]string) string {
	names := make([]string, 0, len(allow))
	for _, actual := range allow {
		names = append(names, actual)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// VerifyTools fails if any allowlisted tool is absent from tools/list. The
// server silently ignores unknown --tools names, and HCP tools only appear
// when the TFE token is accepted, so a failure means a typo or a bad token.
func (c *Client) VerifyTools(ctx context.Context) error {
	listed := map[string]bool{}
	params := &mcp.ListToolsParams{}
	for {
		res, err := c.session.ListTools(ctx, params)
		if err != nil {
			return errors.New("tfmcp: tools/list failed")
		}
		for _, t := range res.Tools {
			listed[t.Name] = true
		}
		if res.NextCursor == "" {
			break
		}
		params = &mcp.ListToolsParams{Cursor: res.NextCursor}
	}
	var missing []string
	for logical, actual := range c.allowlist {
		if !listed[actual] {
			missing = append(missing, logical+"="+actual)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("tfmcp: allowlisted tools missing from server: %s", strings.Join(missing, ", "))
	}
	return nil
}

// Call runs the tool registered under logicalName and returns its text
// content. Rate-limit tool errors are retried with exponential backoff, up to
// maxAttempts attempts. Errors never contain arguments or response text.
func (c *Client) Call(ctx context.Context, logicalName string, args map[string]any) (string, error) {
	actual, ok := c.allowlist[logicalName]
	if !ok {
		return "", fmt.Errorf("tfmcp: tool %q is not allowlisted", logicalName)
	}
	delay := baseBackoff
	for attempt := 1; ; attempt++ {
		text, rateLimited, err := c.callOnce(ctx, actual, args)
		if !rateLimited {
			return text, err
		}
		if attempt == maxAttempts {
			return "", fmt.Errorf("tfmcp: tool %q: rate limited after %d attempts", actual, maxAttempts)
		}
		if err := c.sleep(ctx, delay); err != nil {
			return "", fmt.Errorf("tfmcp: tool %q: waiting to retry: %w", actual, err)
		}
		delay *= 2
	}
}

func (c *Client) callOnce(ctx context.Context, actual string, args map[string]any) (string, bool, error) {
	res, err := c.session.CallTool(ctx, &mcp.CallToolParams{Name: actual, Arguments: args})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", false, fmt.Errorf("tfmcp: tool %q: %w", actual, ctxErr)
		}
		return "", false, fmt.Errorf("tfmcp: tool %q: transport or protocol error", actual)
	}
	text := joinText(res.Content)
	if !res.IsError {
		return text, false, nil
	}
	if strings.Contains(strings.ToLower(text), rateLimitPhase) {
		return "", true, nil
	}
	return "", false, fmt.Errorf("tfmcp: tool %q: server reported a tool error", actual)
}

func joinText(content []mcp.Content) string {
	var b strings.Builder
	for _, c := range content {
		if t, ok := c.(*mcp.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// Close ends the session, waits for the child to exit and removes the temp HOME.
func (c *Client) Close() error {
	err := c.session.Close()
	if rmErr := os.RemoveAll(c.home); rmErr != nil && err == nil {
		err = fmt.Errorf("tfmcp: remove temp HOME: %w", rmErr)
	}
	return err
}
