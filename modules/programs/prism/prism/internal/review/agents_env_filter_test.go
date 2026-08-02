package review_test

// agents_env_filter_test.go — binds the canonical review-agent set to the
// role-aware AgentEnvVars filter (issue #2533).
//
// config.FilterAgentEnvVarsForRole lists the five review roles as literals,
// because internal/review imports internal/config and the reverse import is a
// cycle. Without this test a sixth review agent added to Agents() would
// silently regain the 65-tool grafana surface, which is the exact defect
// #2533 closed.

import (
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/review"
)

// TestReviewAgentsAreCoveredByEnvFilter asserts that every agent returned by
// review.Agents() loses both grafana gate keys when its name goes through the
// env filter.
func TestReviewAgentsAreCoveredByEnvFilter(t *testing.T) {
	fixture := map[string]string{
		"GIT_EDITOR":              "true",
		"GRAFANA_MCP_CONFIG_PATH": "/run/secrets/grafana_config_home",
		"PI_GRAFANA_MCP_BIN":      "/nix/store/abc-mcp-grafana/bin/mcp-grafana",
	}

	agents := review.Agents()
	if len(agents) == 0 {
		t.Fatal("review.Agents() returned no agents")
	}

	for _, ag := range agents {
		got := config.FilterAgentEnvVarsForRole(ag.Name, fixture)
		for _, k := range []string{"GRAFANA_MCP_CONFIG_PATH", "PI_GRAFANA_MCP_BIN"} {
			if _, present := got[k]; present {
				t.Errorf("review agent %q still receives %q — add it to the review-role exclusions in internal/config/agent_env_roles.go (issue #2533)", ag.Name, k)
			}
		}
		if got["GIT_EDITOR"] != "true" {
			t.Errorf("review agent %q lost a non-grafana key; got %v", ag.Name, got)
		}
	}
}
