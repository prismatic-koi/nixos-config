package container

// rlimit_test.go — unit tests for the #2190 RLIMIT_NOFILE clamping rules and
// the apply/restore round-trip. The clamping rules are tested as a pure
// function (ResolveAgentRlimitNofile) so they run on any platform without
// touching process state; the apply test mutates the test process's own
// limits and restores them, choosing values relative to the current limits so
// it never needs privilege.

import (
	"fmt"
	"syscall"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

func TestResolveAgentRlimitNofile(t *testing.T) {
	const bigHostHard = 1 << 20 // 1048576 — typical Linux hard limit

	tests := []struct {
		name            string
		cfgSoft         int
		cfgHard         int
		hostHard        uint64
		wantSoft        uint64
		wantHard        uint64
		wantHardClamped bool
	}{
		{
			name:    "defaults pass through under a big host hard limit",
			cfgSoft: config.DefaultAgentMaxOpenFilesSoft, cfgHard: config.DefaultAgentMaxOpenFilesHard,
			hostHard: bigHostHard,
			wantSoft: config.DefaultAgentMaxOpenFilesSoft, wantHard: config.DefaultAgentMaxOpenFilesHard,
		},
		{
			name:    "configured hard above host hard is clamped with flag",
			cfgSoft: 8192, cfgHard: bigHostHard * 2,
			hostHard: bigHostHard,
			wantSoft: 8192, wantHard: bigHostHard,
			wantHardClamped: true,
		},
		{
			name:    "soft above hard is clamped down to hard",
			cfgSoft: 32768, cfgHard: 16384,
			hostHard: bigHostHard,
			wantSoft: 16384, wantHard: 16384,
		},
		{
			name:    "host hard below both clamps hard then soft follows",
			cfgSoft: 8192, cfgHard: 16384,
			hostHard: 4096,
			wantSoft: 4096, wantHard: 4096,
			wantHardClamped: true,
		},
		{
			name:    "zero values fall back to compiled-in defaults",
			cfgSoft: 0, cfgHard: 0,
			hostHard: bigHostHard,
			wantSoft: config.DefaultAgentMaxOpenFilesSoft, wantHard: config.DefaultAgentMaxOpenFilesHard,
		},
		{
			name:    "negative values fall back to compiled-in defaults",
			cfgSoft: -1, cfgHard: -100,
			hostHard: bigHostHard,
			wantSoft: config.DefaultAgentMaxOpenFilesSoft, wantHard: config.DefaultAgentMaxOpenFilesHard,
		},
		{
			name:    "host hard equal to configured hard is not a clamp",
			cfgSoft: 8192, cfgHard: 16384,
			hostHard: 16384,
			wantSoft: 8192, wantHard: 16384,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveAgentRlimitNofile(tt.cfgSoft, tt.cfgHard, tt.hostHard)
			if got.Soft != tt.wantSoft {
				t.Errorf("Soft: got %d, want %d", got.Soft, tt.wantSoft)
			}
			if got.Hard != tt.wantHard {
				t.Errorf("Hard: got %d, want %d", got.Hard, tt.wantHard)
			}
			if got.HardClamped != tt.wantHardClamped {
				t.Errorf("HardClamped: got %v, want %v", got.HardClamped, tt.wantHardClamped)
			}
			if got.HostHard != tt.hostHard {
				t.Errorf("HostHard: got %d, want %d", got.HostHard, tt.hostHard)
			}
		})
	}
}

// TestApplyAgentRlimitNofile_AppliesAndRestores verifies the apply/restore
// round-trip against the live process: after Apply the process limits equal
// the resolved pair (this is what a child spawned between Apply and restore
// inherits); after restore the soft limit is back to a usable value.
//
// The test picks its target hard limit at or below the current hard limit so
// it never needs privilege. Restoring a lowered hard limit is best-effort
// (unprivileged processes cannot raise it back), so the post-restore
// assertion only checks the soft limit — the hard limit may legitimately
// remain at the applied value for the rest of this test binary's life, which
// is documented behaviour of ApplyAgentRlimitNofile.
func TestApplyAgentRlimitNofile_AppliesAndRestores(t *testing.T) {
	var orig syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &orig); err != nil {
		t.Fatalf("Getrlimit: %v", err)
	}

	targetHard := uint64(16384)
	if orig.Max < targetHard {
		targetHard = orig.Max
	}
	targetSoft := targetHard / 2

	var warnings []string
	warnf := func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	}

	restore := ApplyAgentRlimitNofile(int(targetSoft), int(targetHard), warnf)

	var applied syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &applied); err != nil {
		t.Fatalf("Getrlimit after apply: %v", err)
	}
	if applied.Cur != targetSoft || applied.Max != targetHard {
		t.Errorf("applied limits: got (soft %d, hard %d), want (soft %d, hard %d)",
			applied.Cur, applied.Max, targetSoft, targetHard)
	}
	// targetHard ≤ orig.Max by construction, so no clamp warning may fire.
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}

	restore()

	var restored syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &restored); err != nil {
		t.Fatalf("Getrlimit after restore: %v", err)
	}
	wantSoft := orig.Cur
	if wantSoft > restored.Max {
		wantSoft = restored.Max
	}
	if restored.Cur != wantSoft {
		t.Errorf("restored soft limit: got %d, want %d", restored.Cur, wantSoft)
	}
}

// TestApplyAgentRlimitNofile_WarnsOnHostHardClamp verifies the #2190
// edge-case AC: a configured hard limit above the host hard limit is clamped
// and produces a warning naming both values.
func TestApplyAgentRlimitNofile_WarnsOnHostHardClamp(t *testing.T) {
	var orig syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &orig); err != nil {
		t.Fatalf("Getrlimit: %v", err)
	}
	if orig.Max >= 1<<62 {
		// Host hard limit is effectively unlimited (e.g. RLIM_INFINITY) —
		// a configured int can never exceed it, so the clamp path is
		// unreachable here. The pure-function clamp coverage in
		// TestResolveAgentRlimitNofile still applies.
		t.Skipf("host hard limit %d too large to exceed with an int config value", orig.Max)
	}

	cfgHard := int(orig.Max) + 1000
	cfgSoft := 1024

	var warnings []string
	warnf := func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	}

	restore := ApplyAgentRlimitNofile(cfgSoft, cfgHard, warnf)
	defer restore()

	var applied syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &applied); err != nil {
		t.Fatalf("Getrlimit after apply: %v", err)
	}
	if applied.Max != orig.Max {
		t.Errorf("applied hard limit: got %d, want host hard %d (clamped)", applied.Max, orig.Max)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings: got %d (%v), want exactly 1 clamp warning", len(warnings), warnings)
	}
}
