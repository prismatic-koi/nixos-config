//go:build !darwin

package cmd

// agent_run_sandbox_exec_other.go — non-Darwin stub for runAgentRunSandboxExec.
//
// sandbox-exec is a macOS-only facility; this stub exists purely to satisfy
// the Go compiler on non-Darwin platforms. In practice it is unreachable
// because the dispatch in runAgentRun (agent_run.go) already guards against
// non-Darwin via a runtime.GOOS check:
//
//	case config.IsolationSandboxExec:
//	    if runtime.GOOS != "darwin" {
//	        return fmt.Errorf("prism agent-run: sandbox-exec isolation requires macOS (Darwin); ...")
//	    }
//	    return runAgentRunSandboxExec(...)
//
// The real implementation (Darwin only) lives in
// agent_run_sandbox_exec_darwin.go.

import (
	"fmt"
	"os"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// runAgentRunSandboxExec is a non-Darwin stub that always returns an error.
// The dispatch in runAgentRun already rejects sandbox-exec on non-Darwin via
// a runtime.GOOS check, so this is unreachable in practice.
func runAgentRunSandboxExec(_ string, _ *db.Status, _ time.Time, _ *os.File) error {
	return fmt.Errorf("agent-run: sandbox-exec isolation requires macOS (Darwin); this stub is unreachable")
}
