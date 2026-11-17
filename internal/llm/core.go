// Package llm builds prompts for, and parses responses from, the narrative
// LLM. This file is pure: it has no ADK or network dependency.
package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/terraform-agent/internal/model"
)

const (
	maxFindings = 50
	blockOpen   = "<<<"
	blockClose  = ">>>"
)

// ReviewResource is one changed resource handed to the LLM as review context.
type ReviewResource struct {
	Address      string
	File         string
	ChangedAttrs []string
	Removed      bool
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

// block wraps untrusted text so the model can tell data from instructions.
// Delimiter look-alikes inside the text are removed so it cannot close the block.
func block(label, body string) string {
	body = strings.ReplaceAll(body, blockOpen, "")
	body = strings.ReplaceAll(body, blockClose, "")
	return fmt.Sprintf("%s%s\n%s\n%s\n", blockOpen, label, body, blockClose)
}

// BuildReviewPrompt renders the review prompt from in.
func BuildReviewPrompt(in ReviewInput) string {
	var b strings.Builder
	b.WriteString(reviewInstruction)
	if in.DiffTruncated {
		b.WriteString("\n참고: diff가 잘려 일부만 제공된다.\n")
	}
	for _, r := range in.Resources {
		b.WriteString("\n" + resourceSection(r))
	}
	for _, m := range in.Modules {
		b.WriteString("\n" + block("module", fmt.Sprintf("name=%s source=%s version=%s", m.Name, m.Source, m.Version)))
	}
	for _, c := range in.Checks {
		b.WriteString("\n" + block("check", fmt.Sprintf("tool=%s passed=%t\n%s", c.Tool, c.Passed, c.Summary)))
	}
	return b.String()
}

func resourceSection(r ReviewResource) string {
	var b strings.Builder
	state := "changed"
	if r.Removed {
		state = "removed"
	}
	b.WriteString(block("resource", fmt.Sprintf("address=%s file=%s state=%s attrs=%s",
		r.Address, r.File, state, strings.Join(r.ChangedAttrs, ","))))
	b.WriteString(block("diff "+r.Address, r.DiffExcerpt))
	if r.DocExcerpt == "" {
		b.WriteString("문서 근거 없음: " + r.Address + "\n")
		return b.String()
	}
	label := "doc " + r.Address
	if r.DocTruncated {
		label += " (truncated)"
	}
	b.WriteString(block(label, r.DocExcerpt))
	return b.String()
}

// BuildPlanRisksPrompt renders the plan risk prompt from addresses, actions
// and attribute names only.
func BuildPlanRisksPrompt(s model.PlanSummary) string {
	var b strings.Builder
	b.WriteString(planRisksInstruction)
	fmt.Fprintf(&b, "\n총계: add=%d change=%d destroy=%d replace=%d\n", s.Add, s.Change, s.Destroy, s.Replace)
	if s.Truncated {
		b.WriteString("참고: 목록이 잘려 일부만 제공된다.\n")
	}
	for _, t := range s.Targets {
		b.WriteString(block("target", fmt.Sprintf("address=%s action=%s attrs=%s", t.Address, t.Action, strings.Join(t.Attrs, ","))))
	}
	return b.String()
}

// BuildPlanFailurePrompt renders the plan failure prompt from error blocks.
func BuildPlanFailurePrompt(blocks []string) string {
	var b strings.Builder
	b.WriteString(planFailureInstruction + "\n")
	for _, e := range blocks {
		b.WriteString(block("error", e))
	}
	return b.String()
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
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
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
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
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
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return "", fmt.Errorf("llm: decode failure analysis: %w", err)
	}
	a := strings.TrimSpace(resp.Analysis)
	if a == "" {
		return "", errors.New("llm: empty failure analysis")
	}
	return a, nil
}
