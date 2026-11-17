package build_test

import (
	"testing"

	"github.com/terraform-agent/internal/config"
	"github.com/terraform-agent/internal/plan"
	"github.com/terraform-agent/internal/tfdocs"
)

// diagnosticTools mirror review.diagnosticTools, which is unexported.
var diagnosticTools = []string{"whoami", "getTokenPermissions"}

func TestImageConfig_LoadsAndPassesValidation(t *testing.T) {
	if _, err := config.Load("config.json"); err != nil {
		t.Fatalf("build/config.json must pass config.Load: %v", err)
	}
}

func TestImageConfig_AllowlistCoversEveryToolUsedInCode(t *testing.T) {
	cfg, err := config.Load("config.json")
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	used := []string{
		tfdocs.ToolSearchProviderDocs, tfdocs.ToolProviderDetails, tfdocs.ToolLatestProviderVersion,
		plan.ToolGetRunDetails, plan.ToolGetPlanJSON, plan.ToolGetPlanLogs,
	}
	used = append(used, diagnosticTools...)
	for _, name := range used {
		if cfg.McpToolAllowlist[name] == "" {
			t.Errorf("mcpToolAllowlist has no entry for logical tool %q", name)
		}
	}
	if len(cfg.McpToolAllowlist) != len(used) {
		t.Errorf("allowlist has %d entries, code uses %d; extra tools widen the attack surface", len(cfg.McpToolAllowlist), len(used))
	}
}
