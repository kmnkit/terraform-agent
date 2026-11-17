package plan

import (
	"encoding/json"
	"errors"
)

type runInfo struct {
	status        string
	planID        string
	sha           string
	workspaceName string
	org           string
}

type includedEntry struct {
	ID            string         `json:"id"`
	Type          string         `json:"type"`
	Attributes    map[string]any `json:"attributes"`
	Relationships struct {
		Organization struct {
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		} `json:"organization"`
	} `json:"relationships"`
}

type runDocument struct {
	Data struct {
		Attributes    map[string]any `json:"attributes"`
		Relationships struct {
			Plan      relationID `json:"plan"`
			Workspace relationID `json:"workspace"`
		} `json:"relationships"`
	} `json:"data"`
	Included []includedEntry `json:"included"`
}

type relationID struct {
	Data struct {
		ID string `json:"id"`
	} `json:"data"`
}

// parseRunDetails reads a run payload. Errors never include the payload.
// PROVISIONAL (Phase 0 B): assumes JSON:API with
//   - data.attributes.status and data.relationships.plan.data.id;
//   - the commit SHA in data.attributes["commit-sha"] or in an included
//     ingress-attributes/configuration-versions entry;
//   - data.relationships.workspace.data.id resolving to an included entry of
//     type "workspaces" whose attributes.name is the workspace name and whose
//     org is attributes["organization-name"] or
//     relationships.organization.data.id (HCP org IDs are org names).
func parseRunDetails(payload string) (runInfo, error) {
	var doc runDocument
	if err := json.Unmarshal([]byte(payload), &doc); err != nil {
		return runInfo{}, errors.New("plan: run details are not valid JSON")
	}
	d := runInfo{planID: doc.Data.Relationships.Plan.Data.ID}
	d.status, _ = doc.Data.Attributes["status"].(string)
	d.sha, _ = doc.Data.Attributes["commit-sha"].(string)
	wsID := doc.Data.Relationships.Workspace.Data.ID
	for _, inc := range doc.Included {
		switch {
		case d.sha == "" && (inc.Type == "ingress-attributes" || inc.Type == "configuration-versions"):
			d.sha, _ = inc.Attributes["commit-sha"].(string)
		case wsID != "" && inc.Type == "workspaces" && inc.ID == wsID:
			d.workspaceName, d.org = workspaceIdentity(inc)
		}
	}
	return d, nil
}

func workspaceIdentity(inc includedEntry) (name, org string) {
	name, _ = inc.Attributes["name"].(string)
	org, _ = inc.Attributes["organization-name"].(string)
	if org == "" {
		org = inc.Relationships.Organization.Data.ID
	}
	return name, org
}

// verify fails closed unless the run belongs to the trusted org and the
// workspace named by ref, and was created from src.WantSHA. Identity is
// checked before the SHA so a run of another org or workspace always yields
// ErrWorkspaceMismatch, never ErrSHAMismatch (which callers may treat as an
// older push and skip quietly).
func (d runInfo) verify(src Source, ref RunRef) error {
	if d.workspaceName == "" || d.org == "" {
		return errors.New("plan: run workspace or org not found in run details")
	}
	if d.org != src.Org || d.workspaceName != ref.Workspace {
		return ErrWorkspaceMismatch
	}
	return d.verifySHA(src.WantSHA)
}

// verifySHA wraps ErrSHAMismatch only for a present SHA that differs; a
// missing SHA is a plain error so it can never pass as an older push.
func (d runInfo) verifySHA(want string) error {
	if want == "" {
		return errors.New("plan: expected commit SHA is empty")
	}
	if d.sha == "" {
		return errors.New("plan: run commit SHA not found")
	}
	if d.sha != want {
		return ErrSHAMismatch
	}
	return nil
}

// VerifyRunSHA fails closed: it errors when the run's commit SHA is missing
// or differs from wantSHA.
func VerifyRunSHA(runDetails string, wantSHA string) error {
	d, err := parseRunDetails(runDetails)
	if err != nil {
		return err
	}
	return d.verifySHA(wantSHA)
}
