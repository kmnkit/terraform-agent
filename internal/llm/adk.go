package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/terraform-agent/internal/config"
	domain "github.com/terraform-agent/internal/model"
)

const (
	appName     = "tf-review"
	userID      = "tf-review"
	temperature = float32(0.2)
	// maxOutputTokens bounds one response (findings are capped separately).
	maxOutputTokens = int32(4096)
	// maxLLMCalls: the agent has no tools, so exactly one model call is expected.
	maxLLMCalls = 1
)

// Narrator turns structured review/plan data into Korean prose via an LLM.
type Narrator interface {
	ReviewFindings(ctx context.Context, in ReviewInput) ([]domain.Finding, error)
	PlanRisks(ctx context.Context, s domain.PlanSummary) ([]string, error)
	PlanFailure(ctx context.Context, blocks []string) (string, error)
}

type adkNarrator struct {
	llm model.LLM
}

// NewNarratorWithModel builds a Narrator on top of any model.LLM (used by
// NewVertexNarrator and by tests with a fake model).
func NewNarratorWithModel(m model.LLM) Narrator {
	return adkNarrator{llm: m}
}

// vertexBaseURL mirrors genai's default Vertex endpoint selection so the
// endpoint is pinned and GOOGLE_VERTEX_BASE_URL cannot redirect traffic.
func vertexBaseURL(location string) string {
	switch location {
	case "global":
		return "https://aiplatform.googleapis.com/"
	case "us", "eu":
		return fmt.Sprintf("https://aiplatform.%s.rep.googleapis.com/", location)
	}
	return fmt.Sprintf("https://%s-aiplatform.googleapis.com/", location)
}

// vertexClientConfig pins backend, project, location and endpoint explicitly.
// genai reads env vars (GOOGLE_GENAI_USE_VERTEXAI, GOOGLE_CLOUD_PROJECT,
// GOOGLE_CLOUD_LOCATION, GOOGLE_API_KEY, GOOGLE_VERTEX_BASE_URL, ...) only to
// fill values left empty; explicit values here always win. The http client is
// copied because UseDefaultCredentials wraps its transport.
func vertexClientConfig(cfg config.Config, hc *http.Client) *genai.ClientConfig {
	own := *hc
	return &genai.ClientConfig{
		Backend:     genai.BackendVertexAI,
		Project:     cfg.VertexProject,
		Location:    cfg.VertexLocation,
		HTTPClient:  &own,
		HTTPOptions: genai.HTTPOptions{BaseURL: vertexBaseURL(cfg.VertexLocation)},
	}
}

// NewVertexNarrator builds a Narrator backed by Gemini on Vertex AI using ADC.
func NewVertexNarrator(ctx context.Context, cfg config.Config, hc *http.Client) (Narrator, error) {
	if hc == nil {
		return nil, errors.New("llm: http client is required")
	}
	cc := vertexClientConfig(cfg, hc)
	if err := cc.UseDefaultCredentials(); err != nil {
		return nil, fmt.Errorf("llm: vertex credentials: %w", err)
	}
	m, err := gemini.NewModel(ctx, cfg.Model, cc)
	if err != nil {
		return nil, fmt.Errorf("llm: create gemini model: %w", err)
	}
	return NewNarratorWithModel(m), nil
}

// ReviewFindings asks the model for review findings about in.Resources.
func (n adkNarrator) ReviewFindings(ctx context.Context, in ReviewInput) ([]domain.Finding, error) {
	known := make(map[string]bool, len(in.Resources))
	for _, r := range in.Resources {
		known[r.Address] = true
	}
	raw, err := n.generate(ctx, BuildReviewPrompt(in), findingsSchema())
	if err != nil {
		return nil, err
	}
	return ParseFindings(raw, known)
}

// PlanRisks asks the model for operational risks of a plan summary.
func (n adkNarrator) PlanRisks(ctx context.Context, s domain.PlanSummary) ([]string, error) {
	raw, err := n.generate(ctx, BuildPlanRisksPrompt(s), risksSchema())
	if err != nil {
		return nil, err
	}
	return ParseRisks(raw)
}

// PlanFailure asks the model to explain plan error blocks.
func (n adkNarrator) PlanFailure(ctx context.Context, blocks []string) (string, error) {
	raw, err := n.generate(ctx, BuildPlanFailurePrompt(blocks), failureSchema())
	if err != nil {
		return "", err
	}
	return ParseFailure(raw)
}

// generate runs one tool-less agent turn in a fresh in-memory session and
// returns the final response text. Errors never include prompt or response text.
func (n adkNarrator) generate(ctx context.Context, prompt string, schema *genai.Schema) (string, error) {
	temp := temperature
	a, err := llmagent.New(llmagent.Config{
		Name:                  "narrator",
		Model:                 n.llm,
		OutputSchema:          schema,
		GenerateContentConfig: &genai.GenerateContentConfig{Temperature: &temp, MaxOutputTokens: maxOutputTokens},
	})
	if err != nil {
		return "", fmt.Errorf("llm: create agent: %w", err)
	}
	svc := session.InMemoryService()
	r, err := runner.New(runner.Config{AppName: appName, Agent: a, SessionService: svc})
	if err != nil {
		return "", fmt.Errorf("llm: create runner: %w", err)
	}
	created, err := svc.Create(ctx, &session.CreateRequest{AppName: appName, UserID: userID})
	if err != nil {
		return "", fmt.Errorf("llm: create session: %w", err)
	}
	msg := genai.NewContentFromText(prompt, genai.RoleUser)
	var final strings.Builder
	for ev, err := range r.Run(ctx, userID, created.Session.ID(), msg, agent.RunConfig{MaxLLMCalls: maxLLMCalls}) {
		if err != nil {
			return "", fmt.Errorf("llm: run agent: %w", err)
		}
		if ev.ErrorCode != "" {
			return "", fmt.Errorf("llm: model returned error code %s", ev.ErrorCode)
		}
		if ev.IsFinalResponse() {
			final.WriteString(contentText(ev.Content))
		}
	}
	if final.Len() == 0 {
		return "", errors.New("llm: model returned no final response")
	}
	return final.String(), nil
}

func contentText(c *genai.Content) string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range c.Parts {
		if !p.Thought {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func findingsSchema() *genai.Schema {
	item := &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"resource": {Type: genai.TypeString},
			"severity": {Type: genai.TypeString, Enum: []string{"info", "warning", "error"}},
			"message":  {Type: genai.TypeString},
		},
		Required: []string{"resource", "severity", "message"},
	}
	return &genai.Schema{
		Type:       genai.TypeObject,
		Properties: map[string]*genai.Schema{"findings": {Type: genai.TypeArray, Items: item}},
		Required:   []string{"findings"},
	}
}

func risksSchema() *genai.Schema {
	return &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"risks": {Type: genai.TypeArray, Items: &genai.Schema{Type: genai.TypeString}},
		},
		Required: []string{"risks"},
	}
}

func failureSchema() *genai.Schema {
	return &genai.Schema{
		Type:       genai.TypeObject,
		Properties: map[string]*genai.Schema{"analysis": {Type: genai.TypeString}},
		Required:   []string{"analysis"},
	}
}
