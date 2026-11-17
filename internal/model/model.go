// Package model holds the plain value types shared across packages.
package model

// Finding is one review comment about a resource.
type Finding struct {
	Resource string
	Severity string
	Message  string
	DocRef   string
}

// Target is one resource affected by a plan. Attrs lists changed attribute
// names only, never values.
type Target struct {
	Address string
	Action  string
	Attrs   []string
}

// PlanSummary is the value-free digest of a Terraform plan.
type PlanSummary struct {
	Add       int
	Change    int
	Destroy   int
	Replace   int
	Targets   []Target
	Truncated bool
}

// CheckResult is the outcome of one static check tool.
type CheckResult struct {
	Tool    string
	Passed  bool
	Summary string
}

// ProviderRef identifies a registry provider version.
type ProviderRef struct {
	Namespace string
	Name      string
	Version   string
}
