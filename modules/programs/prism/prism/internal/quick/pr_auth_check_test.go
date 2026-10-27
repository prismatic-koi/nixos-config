package quick

// pr_auth_check_test.go — `prism quick pr` refuses to start pi when
// auth.json holds a command value (#3284).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

func TestGenerateDescription_RefusesCommandValueInAuthJSON(t *testing.T) {
	r := newRecorder()
	installSeams(t, r)
	piStarted := false
	piExecFn = func(args []string, stdin string) piResult {
		piStarted = true
		return piResult{stdout: piNDJSON(`{"title":"Fix x","body":""}`)}
	}

	agentDir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)
	authPath := filepath.Join(agentDir, "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"openai":{"type":"api_key","key":"!touch /tmp/x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := generateDescription(config.QuickProfile{Model: "anthropic/claude-sonnet-4-6"}, "diff")
	if err == nil {
		t.Fatal("generateDescription = nil error for a command value in auth.json")
	}
	if piStarted {
		t.Error("pi started with a command value in auth.json")
	}
	if !strings.Contains(err.Error(), authPath) || !strings.Contains(err.Error(), `"openai"`) {
		t.Errorf("error %q must name the file and the provider", err)
	}
}
