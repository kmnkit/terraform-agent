// Package llm builds prompts for, and parses responses from, the narrative
// LLM. This file is pure: it has no ADK or network dependency.
package llm

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/terraform-agent/internal/model"
)

const (
	maxFindings = 50
	// maxResponseBytes caps the raw model output accepted by the parsers.
	maxResponseBytes = 64 * 1024
	blockOpen        = "<<<"
	blockClose       = ">>>"
)

// ReviewResource is one changed resource handed to the LLM as review context.
type ReviewResource struct {
	Address      string
	File         string
	ChangedAttrs []string
	DiffExcerpt  string
	DocExcerpt   string
	DocTruncated bool
}

// ModuleContext is a module call (source and version only, no docs).
type ModuleContext struct {
	Name    string
	Source  string
	Version string
}

// ReviewInput is the complete, value-free input of a code review prompt.
type ReviewInput struct {
	Resources     []ReviewResource
	Modules       []ModuleContext
	Checks        []model.CheckResult
	DiffTruncated bool
}

// newNonce returns a fresh random hex nonce for one prompt. It is an
// unexported variable only so the exported builders stay thin; tests exercise
// the pure build* functions with a fixed nonce instead.
var newNonce = func() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(fmt.Sprintf("llm: crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(buf[:])
}

var angleNeutralizer = strings.NewReplacer("<", "＜", ">", "＞")

var lineBreaks = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ")

// withNonce injects the per-request nonce into an instruction template.
func withNonce(instruction, nonce string) string {
	return strings.ReplaceAll(instruction, noncePlaceholder, nonce)
}

// block wraps untrusted text in nonce-bearing boundaries so the model can tell
// data from instructions. The nonce is unknown to the author of the text, and
// every < and > in label and body is replaced by a full-width look-alike, so
// the text cannot rebuild a boundary. Labels are collapsed to a single line.
func block(nonce, label, body string) string {
	label = angleNeutralizer.Replace(lineBreaks.Replace(label))
	body = angleNeutralizer.Replace(body)
	return fmt.Sprintf("%sDATA-%s %s\n%s\n%sEND-%s\n", blockOpen, nonce, label, body, blockOpen, nonce)
}

// BuildReviewPrompt renders the review prompt from in with a fresh nonce.
func BuildReviewPrompt(in ReviewInput) string { return buildReviewPrompt(in, newNonce()) }

func buildReviewPrompt(in ReviewInput, nonce string) string {
	var b strings.Builder
	b.WriteString(withNonce(reviewInstruction, nonce))
	if in.DiffTruncated {
		b.WriteString("\n注意: diff は切り詰められており、一部のみ提供される。\n")
	}
	for _, r := range in.Resources {
		b.WriteString("\n" + resourceSection(nonce, r))
	}
	for _, m := range in.Modules {
		b.WriteString("\n" + block(nonce, "module", fmt.Sprintf("name=%s source=%s version=%s", m.Name, m.Source, m.Version)))
	}
	for _, c := range in.Checks {
		b.WriteString("\n" + block(nonce, "check", fmt.Sprintf("tool=%s passed=%t\n%s", c.Tool, c.Passed, c.Summary)))
	}
	return b.String()
}

func resourceSection(nonce string, r ReviewResource) string {
	var b strings.Builder
	doc := "provided"
	if r.DocExcerpt == "" {
		doc = "ドキュメント根拠なし"
	}
	b.WriteString(block(nonce, "resource", fmt.Sprintf("address=%s file=%s attrs=%s doc=%s",
		r.Address, r.File, strings.Join(r.ChangedAttrs, ","), doc)))
	b.WriteString(block(nonce, "diff "+r.Address, r.DiffExcerpt))
	if r.DocExcerpt != "" {
		label := "doc " + r.Address
		if r.DocTruncated {
			label += " (truncated)"
		}
		b.WriteString(block(nonce, label, r.DocExcerpt))
	}
	return b.String()
}

// BuildPlanRisksPrompt renders the plan risk prompt from addresses, actions
// and attribute names only.
func BuildPlanRisksPrompt(s model.PlanSummary) string { return buildPlanRisksPrompt(s, newNonce()) }

func buildPlanRisksPrompt(s model.PlanSummary, nonce string) string {
	var b strings.Builder
	b.WriteString(withNonce(planRisksInstruction, nonce))
	fmt.Fprintf(&b, "\n合計: add=%d change=%d destroy=%d replace=%d\n", s.Add, s.Change, s.Destroy, s.Replace)
	if s.Truncated {
		b.WriteString("注意: 一覧は切り詰められており、一部のみ提供される。\n")
	}
	for _, t := range s.Targets {
		b.WriteString(block(nonce, "target", fmt.Sprintf("address=%s action=%s attrs=%s", t.Address, t.Action, strings.Join(t.Attrs, ","))))
	}
	return b.String()
}

// BuildPlanFailurePrompt renders the plan failure prompt from error blocks.
func BuildPlanFailurePrompt(blocks []string) string {
	return buildPlanFailurePrompt(blocks, newNonce())
}

func buildPlanFailurePrompt(blocks []string, nonce string) string {
	var b strings.Builder
	b.WriteString(withNonce(planFailureInstruction, nonce) + "\n")
	for _, e := range blocks {
		b.WriteString(block(nonce, "error", e))
	}
	return b.String()
}

// decodeResponse enforces the raw size cap, strips one surrounding ```json
// fence (the only lenient shape), and decodes strictly into v.
func decodeResponse(raw string, v any) error {
	if len(raw) > maxResponseBytes {
		return fmt.Errorf("response too large: %d bytes", len(raw))
	}
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") && strings.HasSuffix(raw, "```") && len(raw) >= 6 {
		inner := strings.TrimSpace(raw[3 : len(raw)-3])
		raw = strings.TrimSpace(strings.TrimPrefix(inner, "json"))
	}
	return json.Unmarshal([]byte(raw), v)
}

type findingsResponse struct {
	Findings []struct {
		Resource string `json:"resource"`
		Severity string `json:"severity"`
		Message  string `json:"message"`
	} `json:"findings"`
}

// ParseFindings decodes the findings response, dropping findings that point
// at unknown resources or have empty messages. DocRef is never set here.
func ParseFindings(raw string, knownAddrs map[string]bool) ([]model.Finding, error) {
	var resp findingsResponse
	if err := decodeResponse(raw, &resp); err != nil {
		return nil, fmt.Errorf("llm: decode findings: %w", err)
	}
	out := make([]model.Finding, 0, len(resp.Findings))
	for _, f := range resp.Findings {
		msg := strings.TrimSpace(f.Message)
		if msg == "" || !knownAddrs[f.Resource] {
			continue
		}
		out = append(out, model.Finding{Resource: f.Resource, Severity: normalizeSeverity(f.Severity), Message: msg})
		if len(out) == maxFindings {
			break
		}
	}
	return out, nil
}

func normalizeSeverity(s string) string {
	switch s {
	case "info", "warning", "error":
		return s
	}
	return "info"
}

// ParseRisks decodes the plan risks response, trimming and dropping empties.
func ParseRisks(raw string) ([]string, error) {
	var resp struct {
		Risks []string `json:"risks"`
	}
	if err := decodeResponse(raw, &resp); err != nil {
		return nil, fmt.Errorf("llm: decode risks: %w", err)
	}
	out := make([]string, 0, len(resp.Risks))
	for _, r := range resp.Risks {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out, nil
}

// ParseFailure decodes the plan failure analysis; an empty analysis is an error.
func ParseFailure(raw string) (string, error) {
	var resp struct {
		Analysis string `json:"analysis"`
	}
	if err := decodeResponse(raw, &resp); err != nil {
		return "", fmt.Errorf("llm: decode failure analysis: %w", err)
	}
	a := strings.TrimSpace(resp.Analysis)
	if a == "" {
		return "", errors.New("llm: empty failure analysis")
	}
	return a, nil
}
