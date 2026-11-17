// Package config loads and validates the agent's security-relevant settings
// from a JSON file baked into the image.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const (
	// TfeAddress is the only supported HCP Terraform endpoint.
	TfeAddress = "https://app.terraform.io"
	// DefaultGitlabBaseURL is used when gitlabBaseURL is empty.
	DefaultGitlabBaseURL = "https://gitlab.com"
)

// Limits are the positive size caps applied to data handed to or from the LLM.
type Limits struct {
	DiffBytes        int `json:"diffBytes"`
	DocsBytes        int `json:"docsBytes"`
	PlanSummaryBytes int `json:"planSummaryBytes"`
	LLMOutputBytes   int `json:"llmOutputBytes"`
}

// Waits bounds how long the agent waits for a plan to reach a final state.
type Waits struct {
	Initial time.Duration
	Total   time.Duration
}

// TokenFiles are the mounted file paths holding the API tokens.
type TokenFiles struct {
	TFE    string `json:"tfe"`
	GitLab string `json:"gitlab"`
}

// Config is the validated agent configuration.
type Config struct {
	AllowedGitlabProjectID int64             `json:"allowedGitlabProjectID"`
	GitlabBaseURL          string            `json:"gitlabBaseURL"`
	TfeAddress             string            `json:"tfeAddress"`
	TfeOrganization        string            `json:"tfeOrganization"`
	Workspaces             []string          `json:"workspaces"`
	McpBinaryPath          string            `json:"mcpBinaryPath"`
	McpToolAllowlist       map[string]string `json:"mcpToolAllowlist"`
	Model                  string            `json:"model"`
	Limits                 Limits            `json:"limits"`
	Waits                  Waits             `json:"waits"`
	TokenFiles             TokenFiles        `json:"tokenFiles"`
}

// UnmarshalJSON reads {"initial": "30m", "total": "35m"} using Go duration strings.
func (w *Waits) UnmarshalJSON(data []byte) error {
	var raw struct {
		Initial string `json:"initial"`
		Total   string `json:"total"`
	}
	if err := decodeStrict(data, &raw); err != nil {
		return err
	}
	initial, err := parsePositiveDuration("initial", raw.Initial)
	if err != nil {
		return err
	}
	total, err := parsePositiveDuration("total", raw.Total)
	if err != nil {
		return err
	}
	*w = Waits{Initial: initial, Total: total}
	return nil
}

func parsePositiveDuration(name, s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("waits.%s: %w", name, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("waits.%s must be positive, got %q", name, s)
	}
	return d, nil
}

func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after top-level JSON value")
	}
	return nil
}

// Load reads, strictly decodes, defaults and validates the config at path.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: read %s: %w", path, err)
	}
	var c Config
	if err := decodeStrict(data, &c); err != nil {
		return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if c.GitlabBaseURL == "" {
		c.GitlabBaseURL = DefaultGitlabBaseURL
	}
	if err := validate(c); err != nil {
		return Config{}, fmt.Errorf("config: %s: %w", path, err)
	}
	return c, nil
}

func validate(c Config) error {
	var errs []error
	if c.AllowedGitlabProjectID <= 0 {
		errs = append(errs, errors.New("allowedGitlabProjectID is required and must be positive"))
	}
	if c.TfeAddress != TfeAddress {
		errs = append(errs, fmt.Errorf("tfeAddress must be %s, got %q", TfeAddress, c.TfeAddress))
	}
	errs = append(errs, validateStrings(c)...)
	errs = append(errs, validateLimits(c.Limits)...)
	if c.Waits.Initial <= 0 || c.Waits.Total <= 0 {
		errs = append(errs, errors.New("waits.initial and waits.total are required"))
	}
	return errors.Join(errs...)
}

func validateStrings(c Config) []error {
	var errs []error
	required := map[string]string{
		"tfeOrganization":   c.TfeOrganization,
		"mcpBinaryPath":     c.McpBinaryPath,
		"model":             c.Model,
		"tokenFiles.tfe":    c.TokenFiles.TFE,
		"tokenFiles.gitlab": c.TokenFiles.GitLab,
	}
	for name, v := range required {
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", name))
		}
	}
	if strings.HasSuffix(c.Model, "-latest") {
		errs = append(errs, fmt.Errorf("model must not use a -latest alias, got %q", c.Model))
	}
	errs = append(errs, validateWorkspaces(c.Workspaces)...)
	return append(errs, validateAllowlist(c.McpToolAllowlist)...)
}

func validateWorkspaces(ws []string) []error {
	if len(ws) == 0 {
		return []error{errors.New("workspaces must not be empty")}
	}
	for _, w := range ws {
		if w == "" {
			return []error{errors.New("workspaces must not contain empty names")}
		}
	}
	return nil
}

func validateAllowlist(m map[string]string) []error {
	if len(m) == 0 {
		return []error{errors.New("mcpToolAllowlist must not be empty")}
	}
	for logical, actual := range m {
		if logical == "" || actual == "" {
			return []error{errors.New("mcpToolAllowlist entries must have non-empty names")}
		}
	}
	return nil
}

func validateLimits(l Limits) []error {
	var errs []error
	for name, v := range map[string]int{
		"limits.diffBytes":        l.DiffBytes,
		"limits.docsBytes":        l.DocsBytes,
		"limits.planSummaryBytes": l.PlanSummaryBytes,
		"limits.llmOutputBytes":   l.LLMOutputBytes,
	} {
		if v <= 0 {
			errs = append(errs, fmt.Errorf("%s must be positive, got %d", name, v))
		}
	}
	return errs
}
