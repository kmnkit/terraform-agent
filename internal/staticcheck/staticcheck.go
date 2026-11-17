// Package staticcheck defines the CI static-check artifact contract and parses it.
package staticcheck

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/terraform-agent/internal/model"
)

const (
	// ArtifactName is the plain file name of the artifact produced by the CI static-check job.
	ArtifactName = "tf-review-staticcheck.json"
	// JobName is the name of the CI job that produces the artifact.
	JobName = "tf-review-staticcheck"
	// SummaryUnknown is the summary reported for a known tool absent from the artifact.
	SummaryUnknown = "unknown"
	// MaxSummaryRunes caps a summary, including the truncation marker.
	MaxSummaryRunes = 2000
	// MaxArtifactBytes caps the raw artifact size.
	MaxArtifactBytes = 1 << 20

	supportedVersion = 1
	truncationMarker = "…"
)

// knownTools is the report order of the supported tools.
var knownTools = []string{"fmt", "validate", "tflint", "trivy"}

type artifact struct {
	Version int     `json:"version"`
	Results []entry `json:"results"`
}

type entry struct {
	Tool    string `json:"tool"`
	Passed  bool   `json:"passed"`
	Summary string `json:"summary"`
}

// Parse decodes the artifact into results ordered fmt, validate, tflint, trivy.
// Empty input yields an empty slice; known tools missing from the artifact are
// reported as not passed with SummaryUnknown.
func Parse(raw []byte) ([]model.CheckResult, error) {
	if len(raw) > MaxArtifactBytes {
		return nil, fmt.Errorf("staticcheck: artifact exceeds %d bytes", MaxArtifactBytes)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return []model.CheckResult{}, nil
	}
	art, err := decode(raw)
	if err != nil {
		return nil, err
	}
	byTool, err := indexByTool(art.Results)
	if err != nil {
		return nil, err
	}
	results := make([]model.CheckResult, 0, len(knownTools))
	for _, name := range knownTools {
		e, ok := byTool[name]
		if !ok {
			results = append(results, model.CheckResult{Tool: name, Summary: SummaryUnknown})
			continue
		}
		results = append(results, model.CheckResult{Tool: name, Passed: e.Passed, Summary: truncate(e.Summary)})
	}
	return results, nil
}

func decode(raw []byte) (artifact, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var art artifact
	if err := dec.Decode(&art); err != nil {
		return artifact{}, fmt.Errorf("staticcheck: decode artifact: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return artifact{}, errors.New("staticcheck: unexpected data after artifact JSON")
	}
	if art.Version != supportedVersion {
		return artifact{}, fmt.Errorf("staticcheck: unsupported artifact version %d", art.Version)
	}
	return art, nil
}

func indexByTool(entries []entry) (map[string]entry, error) {
	byTool := make(map[string]entry, len(entries))
	for _, e := range entries {
		if !isKnownTool(e.Tool) {
			return nil, fmt.Errorf("staticcheck: unknown tool %q", e.Tool)
		}
		if _, dup := byTool[e.Tool]; dup {
			return nil, fmt.Errorf("staticcheck: duplicate tool %q", e.Tool)
		}
		byTool[e.Tool] = e
	}
	return byTool, nil
}

func isKnownTool(name string) bool {
	for _, k := range knownTools {
		if k == name {
			return true
		}
	}
	return false
}

func truncate(s string) string {
	runes := []rune(s)
	if len(runes) <= MaxSummaryRunes {
		return s
	}
	keep := MaxSummaryRunes - len([]rune(truncationMarker))
	return string(runes[:keep]) + truncationMarker
}
