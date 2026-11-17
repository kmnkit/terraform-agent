package llm

import (
	"context"
	"errors"
	"iter"
	"net/http"
	"strings"
	"sync"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/terraform-agent/internal/config"
	domain "github.com/terraform-agent/internal/model"
)

// fakeLLM records requests and replies with canned text or an error.
type fakeLLM struct {
	mu   sync.Mutex
	text string
	err  error
	reqs []*model.LLMRequest
}

func (f *fakeLLM) Name() string { return "fake" }

func (f *fakeLLM) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	return func(yield func(*model.LLMResponse, error) bool) {
		if f.err != nil {
			yield(nil, f.err)
			return
		}
		yield(&model.LLMResponse{Content: genai.NewContentFromText(f.text, genai.RoleModel), TurnComplete: true}, nil)
	}
}

func (f *fakeLLM) onlyRequest(t *testing.T) *model.LLMRequest {
	t.Helper()
	if len(f.reqs) != 1 {
		t.Fatalf("model called %d times, want 1", len(f.reqs))
	}
	return f.reqs[0]
}

func requireStructured(t *testing.T, req *model.LLMRequest, prop string) {
	t.Helper()
	if req.Config == nil || req.Config.ResponseMIMEType != "application/json" {
		t.Fatalf("ResponseMIMEType not json: %+v", req.Config)
	}
	if req.Config.ResponseSchema == nil || req.Config.ResponseSchema.Properties[prop] == nil {
		t.Fatalf("ResponseSchema lacks %q", prop)
	}
	if req.Config.Temperature == nil || *req.Config.Temperature > 0.3 {
		t.Errorf("temperature not low: %v", req.Config.Temperature)
	}
	if req.Config.MaxOutputTokens != maxOutputTokens {
		t.Errorf("MaxOutputTokens = %d, want %d", req.Config.MaxOutputTokens, maxOutputTokens)
	}
	if len(req.Tools) != 0 {
		t.Errorf("tools registered: %d", len(req.Tools))
	}
}

func TestReviewFindings_HappyPath(t *testing.T) {
	f := &fakeLLM{text: `{"findings":[{"resource":"google_storage_bucket.logs","severity":"warning","message":"버킷 위치 확인"},{"resource":"ghost.x","severity":"error","message":"없는 리소스"}]}`}
	got, err := NewNarratorWithModel(f).ReviewFindings(context.Background(), sampleInput())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Resource != "google_storage_bucket.logs" || got[0].Severity != "warning" {
		t.Fatalf("got %+v", got)
	}
	req := f.onlyRequest(t)
	requireStructured(t, req, "findings")
	if len(req.Contents) != 1 || len(req.Contents[0].Parts) != 1 {
		t.Fatalf("want one user message with one part: %+v", req.Contents)
	}
	data := req.Contents[0].Parts[0].Text
	if !strings.Contains(data, "DOC-EXCERPT-SENTINEL") || strings.Contains(data, "<OBJECTIVE_AND_PERSONA>") {
		t.Errorf("user message must carry the data blocks only:\n%s", data)
	}
	sys := systemText(req)
	// The JSON example survives verbatim: no session-state templating on {...}.
	if !strings.Contains(sys, "<OBJECTIVE_AND_PERSONA>") || !strings.Contains(sys, reviewExampleOutput) {
		t.Errorf("system instruction missing or mangled:\n%s", sys)
	}
	if strings.Contains(sys, "DOC-EXCERPT-SENTINEL") || strings.Contains(sys, "CHECK-SUMMARY-SENTINEL") {
		t.Error("untrusted text in system instruction")
	}
}

func systemText(req *model.LLMRequest) string {
	if req.Config == nil || req.Config.SystemInstruction == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range req.Config.SystemInstruction.Parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

func TestPlanRisks_HappyPath(t *testing.T) {
	f := &fakeLLM{text: `{"risks":["DB 삭제"]}`}
	s := domain.PlanSummary{Targets: []domain.Target{{Address: "a.b", Action: "delete"}}}
	got, err := NewNarratorWithModel(f).PlanRisks(context.Background(), s)
	if err != nil || len(got) != 1 || got[0] != "DB 삭제" {
		t.Fatalf("got %v err %v", got, err)
	}
	req := f.onlyRequest(t)
	requireStructured(t, req, "risks")
	if !strings.Contains(systemText(req), planRisksExampleOutput) {
		t.Error("risks system instruction missing or mangled")
	}
}

func TestPlanFailure_HappyPath(t *testing.T) {
	f := &fakeLLM{text: `{"analysis":"API 비활성"}`}
	got, err := NewNarratorWithModel(f).PlanFailure(context.Background(), []string{"Error: x"})
	if err != nil || got != "API 비활성" {
		t.Fatalf("got %q err %v", got, err)
	}
	req := f.onlyRequest(t)
	requireStructured(t, req, "analysis")
	if !strings.Contains(systemText(req), planFailureExampleOutput) {
		t.Error("failure system instruction missing or mangled")
	}
}

func TestNarrator_LLMErrorWrappedWithoutPrompt(t *testing.T) {
	f := &fakeLLM{err: errors.New("backend unavailable")}
	n := NewNarratorWithModel(f)
	_, err := n.ReviewFindings(context.Background(), sampleInput())
	if err == nil || !strings.Contains(err.Error(), "backend unavailable") {
		t.Fatalf("got %v", err)
	}
	for _, leak := range []string{"DOC-EXCERPT-SENTINEL", "CHECK-SUMMARY-SENTINEL"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error leaks prompt: %v", err)
		}
	}
	if _, err := n.PlanRisks(context.Background(), domain.PlanSummary{}); err == nil {
		t.Error("PlanRisks: want error")
	}
	if _, err := n.PlanFailure(context.Background(), nil); err == nil {
		t.Error("PlanFailure: want error")
	}
}

func TestNarrator_MalformedOutputDoesNotLeakResponse(t *testing.T) {
	f := &fakeLLM{text: "RESPONSE-SENTINEL not json"}
	n := NewNarratorWithModel(f)
	_, err := n.ReviewFindings(context.Background(), sampleInput())
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "RESPONSE-SENTINEL") {
		t.Errorf("error leaks response: %v", err)
	}
	if _, err := n.PlanRisks(context.Background(), domain.PlanSummary{}); err == nil {
		t.Error("PlanRisks: want error")
	}
	if _, err := n.PlanFailure(context.Background(), nil); err == nil {
		t.Error("PlanFailure: want error")
	}
}

func TestNarrator_FreshSessionPerCall(t *testing.T) {
	f := &fakeLLM{text: `{"risks":[]}`}
	n := NewNarratorWithModel(f)
	for i := 0; i < 2; i++ {
		if _, err := n.PlanRisks(context.Background(), domain.PlanSummary{}); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.reqs) != 2 || len(f.reqs[1].Contents) != 1 {
		t.Fatalf("second call carried history: %d contents", len(f.reqs[1].Contents))
	}
}

func TestVertexClientConfig(t *testing.T) {
	hc := &http.Client{}
	cfg := config.Config{VertexProject: "acme-prod", VertexLocation: "asia-northeast3"}
	cc := vertexClientConfig(cfg, hc)
	if cc.Backend != genai.BackendVertexAI || cc.Project != "acme-prod" || cc.Location != "asia-northeast3" {
		t.Errorf("config: %+v", cc)
	}
	if cc.HTTPOptions.BaseURL != "https://asia-northeast3-aiplatform.googleapis.com/" {
		t.Errorf("BaseURL: %q", cc.HTTPOptions.BaseURL)
	}
	if cc.HTTPClient == hc {
		t.Error("caller's http client must be copied, not mutated")
	}
}

func TestVertexBaseURL(t *testing.T) {
	cases := map[string]string{
		"global":      "https://aiplatform.googleapis.com/",
		"us":          "https://aiplatform.us.rep.googleapis.com/",
		"eu":          "https://aiplatform.eu.rep.googleapis.com/",
		"us-central1": "https://us-central1-aiplatform.googleapis.com/",
	}
	for loc, want := range cases {
		if got := vertexBaseURL(loc); got != want {
			t.Errorf("%s: got %q want %q", loc, got, want)
		}
	}
}

func TestNewVertexNarrator_NilHTTPClient(t *testing.T) {
	if _, err := NewVertexNarrator(context.Background(), config.Config{}, nil); err == nil {
		t.Error("want error for nil http client")
	}
}
