package plan

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"sort"

	"github.com/terraform-agent/internal/model"
)

type resourceChange struct {
	Address string `json:"address"`
	Change  struct {
		Actions      []string                   `json:"actions"`
		Before       map[string]json.RawMessage `json:"before"`
		After        map[string]json.RawMessage `json:"after"`
		AfterUnknown map[string]json.RawMessage `json:"after_unknown"`
	} `json:"change"`
}

// Aggregate reduces a Terraform JSON plan to counts plus, per changed
// resource, the address, action and names of changed top-level attributes.
// Values are only compared, never copied; prior_state, planned_values,
// variables, configuration and output_changes are not decoded at all.
//
// The document must carry a non-empty top-level format_version: any other
// valid JSON (an API wrapper, an error object, {}) would otherwise read as a
// plan without changes. resource_changes is not required, because Terraform
// omits it for a plan with no changes.
func Aggregate(planJSON []byte, maxTargets int) (model.PlanSummary, error) {
	var doc struct {
		FormatVersion   string           `json:"format_version"`
		ResourceChanges []resourceChange `json:"resource_changes"`
	}
	if err := json.Unmarshal(planJSON, &doc); err != nil {
		return model.PlanSummary{}, errors.New("plan: plan JSON is not valid")
	}
	if doc.FormatVersion == "" {
		return model.PlanSummary{}, errors.New("plan: plan JSON has no format_version")
	}
	var sum model.PlanSummary
	var targets []model.Target
	for _, rc := range doc.ResourceChanges {
		action := actionOf(rc.Change.Actions)
		if action == "" {
			continue
		}
		sum = count(sum, action)
		targets = append(targets, model.Target{Address: rc.Address, Action: action, Attrs: changedAttrs(rc)})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Address < targets[j].Address })
	maxTargets = max(maxTargets, 0)
	if len(targets) > maxTargets {
		targets = targets[:maxTargets]
		sum.Truncated = true
	}
	sum.Targets = targets
	return sum, nil
}

// actionOf maps terraform actions to create/update/delete/replace, or ""
// for no-op, read and anything unrecognized.
func actionOf(actions []string) string {
	switch {
	case slices.Equal(actions, []string{"create"}):
		return "create"
	case slices.Equal(actions, []string{"update"}):
		return "update"
	case slices.Equal(actions, []string{"delete"}):
		return "delete"
	case slices.Equal(actions, []string{"delete", "create"}), slices.Equal(actions, []string{"create", "delete"}):
		return "replace"
	}
	return ""
}

func count(s model.PlanSummary, action string) model.PlanSummary {
	switch action {
	case "create":
		s.Add++
	case "update":
		s.Change++
	case "delete":
		s.Destroy++
	case "replace":
		s.Replace++
	}
	return s
}

// changedAttrs returns sorted names whose before/after differ plus names
// that are unknown after apply.
func changedAttrs(rc resourceChange) []string {
	names := map[string]struct{}{}
	for k, after := range rc.Change.After {
		if before, ok := rc.Change.Before[k]; !ok || !sameJSON(before, after) {
			names[k] = struct{}{}
		}
	}
	for k := range rc.Change.Before {
		if _, ok := rc.Change.After[k]; !ok {
			names[k] = struct{}{}
		}
	}
	for k, v := range rc.Change.AfterUnknown {
		if containsTrue(v) {
			names[k] = struct{}{}
		}
	}
	out := make([]string, 0, len(names))
	for k := range names {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sameJSON compares two values after canonical re-marshal (map keys sorted).
// The decoded values live only inside this call.
func sameJSON(a, b json.RawMessage) bool {
	ca, errA := canonical(a)
	cb, errB := canonical(b)
	if errA != nil || errB != nil {
		return false
	}
	return bytes.Equal(ca, cb)
}

// canonical re-marshals with UseNumber so large integers keep full precision.
func canonical(raw json.RawMessage) ([]byte, error) {
	v, err := decodeNumbers(raw)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

func decodeNumbers(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	err := dec.Decode(&v)
	return v, err
}

// containsTrue reports whether an after_unknown value is true at any depth.
func containsTrue(raw json.RawMessage) bool {
	v, err := decodeNumbers(raw)
	return err == nil && anyTrue(v)
}

func anyTrue(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case map[string]any:
		for _, e := range t {
			if anyTrue(e) {
				return true
			}
		}
	case []any:
		for _, e := range t {
			if anyTrue(e) {
				return true
			}
		}
	}
	return false
}
