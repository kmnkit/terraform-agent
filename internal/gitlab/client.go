// Package gitlab is a minimal GitLab REST v4 client bound to one project.
package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	perPage      = 100
	maxPages     = 50
	maxAttempts  = 3
	maxRedirects = 10
	maxBodyBytes = 16 << 20
	baseBackoff  = time.Second
	maxBackoff   = 30 * time.Second
)

// ErrNotFound is returned (wrapped) when GitLab answers 404.
var ErrNotFound = errors.New("gitlab: not found")

type User struct {
	ID       int64
	Username string
}

type MergeRequest struct {
	IID             int
	ProjectID       int64
	SHA             string
	SourceProjectID int64
	TargetProjectID int64
}

type Pipeline struct {
	ID  int64
	SHA string
}

type FileDiff struct {
	OldPath, NewPath, Diff            string
	NewFile, DeletedFile, RenamedFile bool
}

type Job struct {
	ID     int64
	Name   string
	Status string
}

type CommitStatus struct {
	Name, Status, TargetURL, SHA string
}

type Note struct {
	ID       int64
	Body     string
	AuthorID int64
	System   bool
}

// Client talks to a single GitLab project. It is safe for concurrent use.
type Client struct {
	base      string
	token     string
	projectID int64
	hc        *http.Client
	sleep     func(ctx context.Context, d time.Duration) error
}

// NewClient reads the token file once and binds the client to projectID.
func NewClient(baseURL, tokenFile string, projectID int64, hc *http.Client) (*Client, error) {
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("gitlab: read token file: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return nil, errors.New("gitlab: token file is empty")
	}
	origin, err := url.Parse(baseURL)
	if err != nil || origin.Host == "" {
		return nil, errors.New("gitlab: invalid base URL")
	}
	guarded := *hc // shallow copy: the caller's client is left untouched
	guarded.CheckRedirect = guardRedirect(origin)
	return &Client{
		base:      strings.TrimRight(baseURL, "/"),
		token:     token,
		projectID: projectID,
		hc:        &guarded,
		sleep:     sleepCtx,
	}, nil
}

// guardRedirect limits redirects, refuses https->http downgrades and strips
// PRIVATE-TOKEN (which net/http would otherwise copy) when the target is not
// the configured GitLab origin, e.g. object storage behind artifact downloads.
func guardRedirect(origin *url.URL) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		if via[len(via)-1].URL.Scheme == "https" && req.URL.Scheme != "https" {
			return errors.New("refusing https to http redirect")
		}
		if req.URL.Scheme != origin.Scheme || req.URL.Host != origin.Host {
			req.Header.Del("PRIVATE-TOKEN")
		}
		return nil
	}
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

func (c *Client) projectPath(format string, args ...any) string {
	return fmt.Sprintf("/projects/%d", c.projectID) + fmt.Sprintf(format, args...)
}

// do performs one logical request with bounded retries on 429 and 5xx. Errors
// carry only the endpoint name and status, never URLs, bodies or the token.
func (c *Client) do(ctx context.Context, name, method, path string, query url.Values, body []byte) ([]byte, http.Header, error) {
	target := c.base + "/api/v4" + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	for attempt := 1; ; attempt++ {
		data, hdr, status, err := c.once(ctx, name, method, target, body)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case status == http.StatusNotFound:
			return nil, nil, fmt.Errorf("gitlab %s: status 404: %w", name, ErrNotFound)
		case status == http.StatusTooManyRequests || status >= 500:
			if attempt >= maxAttempts {
				return nil, nil, fmt.Errorf("gitlab %s: status %d after %d attempts", name, status, attempt)
			}
			if err := c.sleep(ctx, backoff(hdr, attempt)); err != nil {
				return nil, nil, fmt.Errorf("gitlab %s: wait for retry: %w", name, err)
			}
		case status >= 300:
			return nil, nil, fmt.Errorf("gitlab %s: status %d", name, status)
		default:
			return data, hdr, nil
		}
	}
}

func (c *Client) once(ctx context.Context, name, method, target string, body []byte) ([]byte, http.Header, int, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("gitlab %s: build request failed (invalid base URL?)", name)
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		// url.Error embeds the URL; report only the underlying cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, nil, 0, fmt.Errorf("gitlab %s: request failed: %w", name, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, nil, 0, fmt.Errorf("gitlab %s: read response: %w", name, err)
	}
	if len(data) > maxBodyBytes {
		return nil, nil, 0, fmt.Errorf("gitlab %s: response exceeds %d bytes", name, maxBodyBytes)
	}
	return data, resp.Header, resp.StatusCode, nil
}

func backoff(h http.Header, attempt int) time.Duration {
	if s, err := strconv.Atoi(h.Get("Retry-After")); err == nil && s >= 0 {
		return min(time.Duration(s)*time.Second, maxBackoff)
	}
	return min(baseBackoff<<(attempt-1), maxBackoff)
}

func getJSON[T any](ctx context.Context, c *Client, name, path string) (T, error) {
	var out T
	data, _, err := c.do(ctx, name, http.MethodGet, path, nil, nil)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("gitlab %s: decode response: %w", name, err)
	}
	return out, nil
}

// listAll follows X-Next-Page; exceeding maxPages is an error, not truncation.
func listAll[T any](ctx context.Context, c *Client, name, path string) ([]T, error) {
	var all []T
	page := "1"
	for i := 0; i < maxPages; i++ {
		q := url.Values{"per_page": {strconv.Itoa(perPage)}, "page": {page}}
		data, hdr, err := c.do(ctx, name, http.MethodGet, path, q, nil)
		if err != nil {
			return nil, err
		}
		var items []T
		if err := json.Unmarshal(data, &items); err != nil {
			return nil, fmt.Errorf("gitlab %s: decode response: %w", name, err)
		}
		all = append(all, items...)
		if page = hdr.Get("X-Next-Page"); page == "" {
			return all, nil
		}
	}
	return nil, fmt.Errorf("gitlab %s: more than %d pages", name, maxPages)
}

func (c *Client) CurrentUser(ctx context.Context) (User, error) {
	r, err := getJSON[struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	}](ctx, c, "CurrentUser", "/user")
	return User{ID: r.ID, Username: r.Username}, err
}

func (c *Client) MergeRequest(ctx context.Context, iid int) (MergeRequest, error) {
	r, err := getJSON[struct {
		IID             int    `json:"iid"`
		ProjectID       int64  `json:"project_id"`
		SHA             string `json:"sha"`
		SourceProjectID int64  `json:"source_project_id"`
		TargetProjectID int64  `json:"target_project_id"`
	}](ctx, c, "MergeRequest", c.projectPath("/merge_requests/%d", iid))
	return MergeRequest{IID: r.IID, ProjectID: r.ProjectID, SHA: r.SHA,
		SourceProjectID: r.SourceProjectID, TargetProjectID: r.TargetProjectID}, err
}

func (c *Client) MergeRequestPipelines(ctx context.Context, iid int) ([]Pipeline, error) {
	type raw struct {
		ID  int64  `json:"id"`
		SHA string `json:"sha"`
	}
	rs, err := listAll[raw](ctx, c, "MergeRequestPipelines", c.projectPath("/merge_requests/%d/pipelines", iid))
	out := make([]Pipeline, len(rs))
	for i, r := range rs {
		out[i] = Pipeline{ID: r.ID, SHA: r.SHA}
	}
	return out, err
}

func (c *Client) MergeRequestDiffs(ctx context.Context, iid int) ([]FileDiff, error) {
	type raw struct {
		OldPath     string `json:"old_path"`
		NewPath     string `json:"new_path"`
		Diff        string `json:"diff"`
		NewFile     bool   `json:"new_file"`
		DeletedFile bool   `json:"deleted_file"`
		RenamedFile bool   `json:"renamed_file"`
	}
	rs, err := listAll[raw](ctx, c, "MergeRequestDiffs", c.projectPath("/merge_requests/%d/diffs", iid))
	out := make([]FileDiff, len(rs))
	for i, r := range rs {
		out[i] = FileDiff(r)
	}
	return out, err
}

// FileAtRef returns the raw file content at ref. The path is URL-escaped as a
// single segment, as the repository files API requires.
func (c *Client) FileAtRef(ctx context.Context, path, ref string) ([]byte, error) {
	p := c.projectPath("/repository/files/%s/raw", url.PathEscape(path))
	data, _, err := c.do(ctx, "FileAtRef", http.MethodGet, p, url.Values{"ref": {ref}}, nil)
	return data, err
}

func (c *Client) PipelineJobs(ctx context.Context, pipelineID int64) ([]Job, error) {
	type raw struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	rs, err := listAll[raw](ctx, c, "PipelineJobs", c.projectPath("/pipelines/%d/jobs", pipelineID))
	out := make([]Job, len(rs))
	for i, r := range rs {
		out[i] = Job(r)
	}
	return out, err
}

// JobArtifact returns one artifact file; a missing artifact yields ErrNotFound.
func (c *Client) JobArtifact(ctx context.Context, jobID int64, artifactPath string) ([]byte, error) {
	segs := strings.Split(artifactPath, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	p := c.projectPath("/jobs/%d/artifacts/%s", jobID, strings.Join(segs, "/"))
	data, _, err := c.do(ctx, "JobArtifact", http.MethodGet, p, nil, nil)
	return data, err
}

func (c *Client) CommitStatuses(ctx context.Context, sha string) ([]CommitStatus, error) {
	type raw struct {
		Name      string `json:"name"`
		Status    string `json:"status"`
		TargetURL string `json:"target_url"`
		SHA       string `json:"sha"`
	}
	rs, err := listAll[raw](ctx, c, "CommitStatuses", c.projectPath("/repository/commits/%s/statuses", url.PathEscape(sha)))
	out := make([]CommitStatus, len(rs))
	for i, r := range rs {
		out[i] = CommitStatus(r)
	}
	return out, err
}

type rawNote struct {
	ID     int64  `json:"id"`
	Body   string `json:"body"`
	System bool   `json:"system"`
	Author struct {
		ID int64 `json:"id"`
	} `json:"author"`
}

func (r rawNote) note() Note {
	return Note{ID: r.ID, Body: r.Body, AuthorID: r.Author.ID, System: r.System}
}

func (c *Client) ListNotes(ctx context.Context, iid int) ([]Note, error) {
	rs, err := listAll[rawNote](ctx, c, "ListNotes", c.projectPath("/merge_requests/%d/notes", iid))
	out := make([]Note, len(rs))
	for i, r := range rs {
		out[i] = r.note()
	}
	return out, err
}

func noteBody(body string) []byte {
	b, _ := json.Marshal(map[string]string{"body": body})
	return b
}

func (c *Client) CreateNote(ctx context.Context, iid int, body string) (Note, error) {
	data, _, err := c.do(ctx, "CreateNote", http.MethodPost, c.projectPath("/merge_requests/%d/notes", iid), nil, noteBody(body))
	if err != nil {
		return Note{}, err
	}
	var r rawNote
	if err := json.Unmarshal(data, &r); err != nil {
		return Note{}, fmt.Errorf("gitlab CreateNote: decode response: %w", err)
	}
	return r.note(), nil
}

func (c *Client) UpdateNote(ctx context.Context, iid int, noteID int64, body string) error {
	_, _, err := c.do(ctx, "UpdateNote", http.MethodPut, c.projectPath("/merge_requests/%d/notes/%d", iid, noteID), nil, noteBody(body))
	return err
}

func (c *Client) DeleteNote(ctx context.Context, iid int, noteID int64) error {
	_, _, err := c.do(ctx, "DeleteNote", http.MethodDelete, c.projectPath("/merge_requests/%d/notes/%d", iid, noteID), nil, nil)
	return err
}
