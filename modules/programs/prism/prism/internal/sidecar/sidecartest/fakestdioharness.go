package sidecartest

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const fakeStdioHarnessPkg = "github.com/prismatic-koi/prism/internal/sidecar/sidecartest/testdata/fakestdioharness"

// FakeStdioHarness builds the fake stdio harness (testdata/fakestdioharness)
// into a t.TempDir() and returns the path of the binary. Use it as
// Config.HarnessBinaryPath. Set PRISM_FAKE_STDIO_HARNESS to select the
// frames that the harness writes.
//
// Do not use the test binary as the harness. buildStdioHarnessCmd binds only
// /nix, /etc, /bin, /run/current-system and the directory of the harness.
// A go test -race binary needs cgo, so it is dynamic. On a host that is not
// NixOS its ELF interpreter is /lib64/ld-linux-x86-64.so.2, and bwrap fails
// with "execvp ...: No such file or directory". CGO_ENABLED=0 makes this
// binary static.
//
// A build failure is fatal, not a skip: go test always has a go toolchain.
func FakeStdioHarness(t testing.TB) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fakestdioharness")
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", bin, fakeStdioHarnessPkg)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake stdio harness: %v\n%s", err, out)
	}
	return bin
}
