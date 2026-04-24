package archive

import (
	"os/exec"
	"runtime/debug"
	"strings"
)

// PrismGitSHA returns the VCS revision embedded in the binary by `go build`,
// or "" when not available (e.g. built with `go run` or in tests).
func PrismGitSHA() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}
	return ""
}

// HarnessVersion returns the version string reported by the opencode binary
// (e.g. "1.1.30"), or "" when the binary is not on PATH or returns an error.
// The result is derived by running `opencode --version` and stripping any
// trailing newline.
func HarnessVersion() string {
	out, err := exec.Command("opencode", "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
