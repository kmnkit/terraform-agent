// Package plan identifies the HCP Terraform run behind a commit, waits for it,
// and reduces its plan to a value-free summary.
package plan

import (
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/terraform-agent/internal/gitlab"
)

// Source pins which HCP Terraform runs are trusted.
type Source struct {
	Host       string
	Org        string
	Workspaces []string
}

// RunRef points at one trusted HCP run. StatusState is the GitLab commit
// status state that carried the link.
type RunRef struct {
	Workspace   string
	RunID       string
	StatusState string
}

var runIDPattern = regexp.MustCompile(`^run-[A-Za-z0-9]+$`)

// FindRun returns the first status whose target_url is a trusted run URL.
// PROVISIONAL (Phase 0 B): the GitLab API is assumed to return statuses
// newest first, so the first match in slice order is the latest run.
func FindRun(statuses []gitlab.CommitStatus, src Source) (RunRef, bool) {
	for _, s := range statuses {
		if ws, id, ok := parseRunURL(s.TargetURL, src); ok {
			return RunRef{Workspace: ws, RunID: id, StatusState: s.Status}, true
		}
	}
	return RunRef{}, false
}

// parseRunURL accepts only the two URL forms below, matched exactly.
// PROVISIONAL (Phase 0 B): the real HCP status target_url shape is unverified.
//
//	https://<Host>/app/<Org>/<ws>/runs/<run-id>
//	https://<Host>/app/<Org>/workspaces/<ws>/runs/<run-id>
func parseRunURL(raw string, src Source) (workspace, runID string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != src.Host || u.User != nil {
		return "", "", false
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(u.EscapedPath(), "%") {
		return "", "", false
	}
	seg := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(seg) == 6 && seg[2] == "workspaces" {
		seg = append(seg[:2:2], seg[3:]...)
	}
	if len(seg) != 5 || seg[0] != "app" || seg[1] != src.Org || seg[3] != "runs" {
		return "", "", false
	}
	if !slices.Contains(src.Workspaces, seg[2]) || !runIDPattern.MatchString(seg[4]) {
		return "", "", false
	}
	return seg[2], seg[4], true
}
