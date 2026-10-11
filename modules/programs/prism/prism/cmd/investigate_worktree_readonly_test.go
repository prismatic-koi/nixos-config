package cmd

import (
	"testing"

	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// TestBuildInvestigateSpawnOpts_WorktreeReadOnly: every investigator is
// spawned with a read-only worktree (#3079), so it cannot change the
// worktree of its invoker.
func TestBuildInvestigateSpawnOpts_WorktreeReadOnly(t *testing.T) {
	for _, mode := range []string{"bwrap", "sandbox-exec", "host"} {
		t.Run(mode, func(t *testing.T) {
			bus := sidecartest.NewIsolated(t, "")
			invoker := "prism-test@coord-readonly-" + mode
			seedInvestigateInvoker(t, bus.DB, invoker, mode)

			opts, _, _, err := buildInvestigateSpawnOpts(bus.DB, invoker, "trace the call chain", "readonly")
			if err != nil {
				t.Fatalf("buildInvestigateSpawnOpts: %v", err)
			}
			if !opts.WorktreeReadOnly {
				t.Error("SpawnOpts.WorktreeReadOnly = false, want true for an investigator")
			}
		})
	}
}
