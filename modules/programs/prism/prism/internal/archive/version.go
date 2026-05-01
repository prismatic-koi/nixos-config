package archive

import (
	"runtime/debug"
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
