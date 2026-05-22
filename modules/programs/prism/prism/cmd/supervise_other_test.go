//go:build !linux

package cmd

// Non-Linux stub for the SuperviseChild test-helper subprocess
// dispatcher. The real implementation lives in
// supervise_linux_test.go behind a //go:build linux tag.
//
// The package-wide TestMain (killsidecar_test.go) calls
// runSuperviseHelper unconditionally when PRISM_TEST_SUPERVISE_HELPER=1
// is set in the environment. On platforms other than Linux the
// helper machinery relies on /dev/ptmx, TIOCSPTLCK / TIOCGPTN, and
// the Setctty / Ctty fields of syscall.SysProcAttr — none of which
// are portable. Rather than silently no-op, this stub exits with
// sentinel code 99 so any accidental cross-platform invocation is
// immediately visible (the Linux build returns codes in the 0–28
// range, so 99 cannot collide with a real verdict).

import (
	"fmt"
	"os"
	"runtime"
)

// runSuperviseHelper is the non-Linux stub.
func runSuperviseHelper() int {
	fmt.Fprintf(os.Stderr, "supervise helper subprocess is Linux-only (GOOS=%s)\n", runtime.GOOS)
	return 99
}
