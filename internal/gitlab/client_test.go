package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testToken = "glpat-secret-token"

func tokenFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func newTestClient(t *testing.T, h http.Handler) (*Client, *[]time.Duration) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, tokenFile(t, " "+testToken+"\n"), 42, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	var slept []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}
	return c, &slept
}

func TestNewClientTokenErrors(t *testing.T) {
	if _, err := NewClient("http://x", filepath.Join(t.TempDir(), "none"), 1, http.DefaultClient); err == nil {
		t.Fatal("missing token file must fail")
	}
	if _, err := NewClient("http://x", tokenFile(t, " \n"), 1, http.DefaultClient); err == nil {
		t.Fatal("empty token must fail")
	}
}

func TestTokenOnlyInHeaderAndPaths(t *testing.T) {
	var gotPaths []string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != testToken {
			t.Errorf("token header = %q", r.Header.Get("PRIVATE-TOKEN"))
		}
		if strings.Contains(r.URL.String(), testToken) {
			t.Errorf("token leaked into URL")
		}
		gotPaths = append(gotPaths, r.URL.EscapedPath())
		switch {
		case r.URL.Path == "/api/v4/user":
			fmt.Fprint(w, `{"id":7,"username":"bot"}`)
		case strings.HasSuffix(r.URL.Path, "/merge_requests/3"):
			fmt.Fprint(w, `{"iid":3,"project_id":42,"sha":"abc","source_project_id":42,"target_project_id":42}`)
		case strings.Contains(r.URL.EscapedPath(), "/repository/files/"):
			if r.URL.Query().Get("ref") != "abc" {
				t.Errorf("ref = %q", r.URL.Query().Get("ref"))
			}
			fmt.Fprint(w, "content")
		case strings.Contains(r.URL.EscapedPath(), "/artifacts/"):
			fmt.Fprint(w, "{}")
		default:
			fmt.Fprint(w, `[]`)
		}
	}))
	ctx := context.Background()
	u, err := c.CurrentUser(ctx)
	if err != nil || u != (User{ID: 7, Username: "bot"}) {
		t.Fatalf("CurrentUser = %+v, %v", u, err)
	}
	mr, err := c.MergeRequest(ctx, 3)
	if err != nil || mr.SHA != "abc" || mr.IID != 3 || mr.ProjectID != 42 {
		t.Fatalf("MergeRequest = %+v, %v", mr, err)
	}
	b, err := c.FileAtRef(ctx, "envs/prod/main.tf", "abc")
	if err != nil || string(b) != "content" {
		t.Fatalf("FileAtRef = %q, %v", b, err)
	}
	if _, err := c.JobArtifact(ctx, 9, "out/plan.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.MergeRequestPipelines(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PipelineJobs(ctx, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CommitStatuses(ctx, "abc"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/api/v4/user",
		"/api/v4/projects/42/merge_requests/3",
		"/api/v4/projects/42/repository/files/envs%2Fprod%2Fmain.tf/raw",
		"/api/v4/projects/42/jobs/9/artifacts/out/plan.json",
		"/api/v4/projects/42/merge_requests/3/pipelines",
		"/api/v4/projects/42/pipelines/5/jobs",
		"/api/v4/projects/42/repository/commits/abc/statuses",
	}
	for i, w := range want {
		if gotPaths[i] != w {
			t.Errorf("path[%d] = %q, want %q", i, gotPaths[i], w)
		}
	}
}

func TestDiffsPagination(t *testing.T) {
	var pages []string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("page")
		pages = append(pages, p)
		if r.URL.Query().Get("per_page") != "100" {
			t.Errorf("per_page = %q", r.URL.Query().Get("per_page"))
		}
		switch p {
		case "", "1":
			w.Header().Set("X-Next-Page", "2")
			fmt.Fprint(w, `[{"old_path":"a.tf","new_path":"a.tf","diff":"d1","new_file":true}]`)
		default:
			fmt.Fprint(w, `[{"old_path":"b.tf","new_path":"c.tf","diff":"d2","renamed_file":true,"deleted_file":false}]`)
		}
	}))
	got, err := c.MergeRequestDiffs(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[0].NewFile || !got[1].RenamedFile || got[1].NewPath != "c.tf" {
		t.Fatalf("diffs = %+v", got)
	}
	if len(pages) != 2 {
		t.Fatalf("pages requested = %v", pages)
	}
}

func TestPaginationCap(t *testing.T) {
	var calls atomic.Int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		w.Header().Set("X-Next-Page", fmt.Sprint(n+1))
		fmt.Fprint(w, `[]`)
	}))
	if _, err := c.ListNotes(context.Background(), 3); err == nil {
		t.Fatal("endless pagination must fail")
	}
	if calls.Load() != int32(maxPages) {
		t.Fatalf("calls = %d, want %d", calls.Load(), maxPages)
	}
}

func TestRetryThenSuccess(t *testing.T) {
	var calls atomic.Int32
	c, slept := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{"id":1,"username":"bot"}`)
	}))
	if _, err := c.CurrentUser(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || len(*slept) != 1 || (*slept)[0] != 3*time.Second {
		t.Fatalf("calls=%d slept=%v", calls.Load(), *slept)
	}
}

func TestRetryExhaustedNoLeak(t *testing.T) {
	var calls atomic.Int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, "secret body "+testToken)
	}))
	_, err := c.CurrentUser(context.Background())
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3", calls.Load())
	}
	if strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), "secret body") {
		t.Fatalf("error leaks: %v", err)
	}
}

func TestNoRetryOn4xx(t *testing.T) {
	var calls atomic.Int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	if _, err := c.CurrentUser(context.Background()); err == nil || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestNotFoundSentinel(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	ctx := context.Background()
	if _, err := c.JobArtifact(ctx, 1, "plan.json"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("JobArtifact err = %v", err)
	}
	if err := c.DeleteNote(ctx, 3, 9); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteNote err = %v", err)
	}
	if err := c.UpdateNote(ctx, 3, 9, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateNote err = %v", err)
	}
}

func TestArtifactSizeCap(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("a"), maxBodyBytes+1))
	}))
	if _, err := c.JobArtifact(context.Background(), 1, "plan.json"); err == nil {
		t.Fatal("oversized artifact must fail")
	}
}

func TestNotesCRUD(t *testing.T) {
	var methods []string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		switch r.Method {
		case http.MethodGet:
			fmt.Fprint(w, `[{"id":5,"body":"b","author":{"id":7},"system":true}]`)
		case http.MethodPost, http.MethodPut:
			raw, _ := io.ReadAll(r.Body)
			var in struct{ Body string }
			_ = json.Unmarshal(raw, &in)
			if in.Body != "hello" {
				t.Errorf("body = %q", in.Body)
			}
			fmt.Fprint(w, `{"id":6,"body":"hello","author":{"id":7},"system":false}`)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	ctx := context.Background()
	notes, err := c.ListNotes(ctx, 3)
	if err != nil || len(notes) != 1 || notes[0] != (Note{ID: 5, Body: "b", AuthorID: 7, System: true}) {
		t.Fatalf("ListNotes = %+v, %v", notes, err)
	}
	n, err := c.CreateNote(ctx, 3, "hello")
	if err != nil || n.ID != 6 || n.AuthorID != 7 {
		t.Fatalf("CreateNote = %+v, %v", n, err)
	}
	if err := c.UpdateNote(ctx, 3, 6, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteNote(ctx, 3, 6); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GET /api/v4/projects/42/merge_requests/3/notes",
		"POST /api/v4/projects/42/merge_requests/3/notes",
		"PUT /api/v4/projects/42/merge_requests/3/notes/6",
		"DELETE /api/v4/projects/42/merge_requests/3/notes/6",
	}
	for i, w := range want {
		if methods[i] != w {
			t.Errorf("call[%d] = %q, want %q", i, methods[i], w)
		}
	}
}

func TestPipelineJobsAndStatusesDecode(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/jobs") {
			fmt.Fprint(w, `[{"id":1,"name":"plan","status":"success"}]`)
			return
		}
		fmt.Fprint(w, `[{"name":"tfc","status":"success","target_url":"https://u","sha":"abc"}]`)
	}))
	jobs, err := c.PipelineJobs(context.Background(), 5)
	if err != nil || jobs[0] != (Job{ID: 1, Name: "plan", Status: "success"}) {
		t.Fatalf("jobs = %+v, %v", jobs, err)
	}
	st, err := c.CommitStatuses(context.Background(), "abc")
	if err != nil || st[0] != (CommitStatus{Name: "tfc", Status: "success", TargetURL: "https://u", SHA: "abc"}) {
		t.Fatalf("statuses = %+v, %v", st, err)
	}
}
