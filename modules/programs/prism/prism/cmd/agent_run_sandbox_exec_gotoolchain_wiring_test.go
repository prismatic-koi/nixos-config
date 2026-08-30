package cmd

// agent_run_sandbox_exec_gotoolchain_wiring_test.go — source-level wiring
// guard for the GOTOOLCHAIN pin.
//
// The dispatcher that assembles the sandbox environment,
// agent_run_sandbox_exec_darwin.go, is behind a darwin build tag, so its
// runtime behaviour is invisible to the Linux CI runners. The env assembly
// itself is not reachable from a unit test either: it ends in syscall.Exec of
// /usr/bin/sandbox-exec.
//
// That leaves the call site unpinned by anything runnable. A source-level
// assertion is cheap, runs on every platform including CI, and fails the
// moment the wiring is deleted or renamed.
//
// This deliberately does NOT re-test what GoToolchainEnv returns — that is
// container.TestGoToolchainEnv_PinsLocal's job. This test asserts only that
// the dispatcher calls it.

import (
	"os"
	"strings"
	"testing"
)

// dispatcherSource is the Darwin sandbox-exec dispatcher, read as text.
const dispatcherSource = "agent_run_sandbox_exec_darwin.go"

// TestSandboxExecDispatcher_InjectsGoToolchainEnv asserts that the Darwin
// dispatcher wires container.GoToolchainEnv() into the sandbox environment.
//
// Without this call the sandbox inherits Go's default GOTOOLCHAIN=auto. Under
// auto, cmd/go downloads a newer toolchain into GOMODCACHE and execs
// <dir>/bin/go from it — which the section-22 deny in the SBPL profile blocks,
// breaking `go build ./...` with "go: exec go1.X.Y: operation not permitted"
// on any repo whose go.mod outgrows the nix-pinned toolchain.
func TestSandboxExecDispatcher_InjectsGoToolchainEnv(t *testing.T) {
	src, err := os.ReadFile(dispatcherSource)
	if err != nil {
		t.Fatalf("read %s: %v", dispatcherSource, err)
	}

	const want = "container.GoToolchainEnv()"
	if !strings.Contains(string(src), want) {
		t.Errorf("%s does not call %s — the sandbox would inherit Go's default "+
			"GOTOOLCHAIN=auto, and the section-22 module-cache exec deny would then break "+
			"`go build` on any repo whose go.mod outgrows the nix-pinned toolchain (issue #2621).",
			dispatcherSource, want)
	}

	// The call must reach the env slice that is handed to the sandbox, not sit
	// in a comment or a dead branch. Assert the append form specifically.
	const wantAppend = "env = append(env, container.GoToolchainEnv()...)"
	if !strings.Contains(string(src), wantAppend) {
		t.Errorf("%s calls GoToolchainEnv but not as %q — verify the value actually reaches "+
			"the sandbox environment.", dispatcherSource, wantAppend)
	}
}
