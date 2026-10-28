package container

import (
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The Go side of the pi auth sync copies values of the anthropic-oauth
// extension: the name and the stale time of the shared refresh lock, the
// form of its owner file, the env var that turns the sync on, and the route
// of the sync. If one side changes alone, the extension does not ask for a
// sync, or the timer sync runs in the middle of a refresh of the extension.
// Then two sessions send the same refresh token again (#3289, #3295).
func TestPIAuthSync_ExtensionUsesSameValues(t *testing.T) {
	const extDir = "../../../pi/extensions/anthropic-oauth"
	if _, err := os.Stat(extDir); os.IsNotExist(err) {
		t.Skipf("%s not present (expected in the nix build sandbox)", extDir)
	}
	read := func(name string) string {
		t.Helper()
		src, err := os.ReadFile(extDir + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(src)
	}
	lock := read("refresh-lock.ts")
	sync := read("session-sync.ts")

	for file, want := range map[string]map[string]string{
		"refresh-lock.ts": {
			"REFRESH_LOCK_NAME": `export const REFRESH_LOCK_NAME = ` + strconv.Quote(piRefreshLockName),
			"owner file":        `JSON.stringify({ id, pid: process.pid, ts: Date.now() })`,
		},
		"session-sync.ts": {
			"SESSION_SYNC_PATH": `export const SESSION_SYNC_PATH = ` + strconv.Quote(PIAuthSyncPath),
			"SESSION_SYNC_ENV":  `export const SESSION_SYNC_ENV = ` + strconv.Quote(PIAuthSyncEnvVar),
		},
	} {
		src := lock
		if file == "session-sync.ts" {
			src = sync
		}
		for what, line := range want {
			if !strings.Contains(src, line) {
				t.Errorf("%s: %s differs from Go: want %s", file, what, line)
			}
		}
	}

	m := regexp.MustCompile(`staleMs:\s*([0-9_]+),`).FindStringSubmatch(lock)
	if m == nil {
		t.Fatalf("refresh-lock.ts: no LOCK_TIMINGS.staleMs")
	}
	ms, err := strconv.ParseInt(strings.ReplaceAll(m[1], "_", ""), 10, 64)
	if err != nil || ms != piRefreshLockStale.Milliseconds() {
		t.Errorf("refresh-lock.ts LOCK_TIMINGS.staleMs = %s, Go piRefreshLockStale = %v", m[1], piRefreshLockStale)
	}

	// The Go owner file has the fields that the extension writes and reads.
	owner, _ := json.Marshal(piNotionLockOwner{ID: "x", PID: 1, TS: 2})
	var fields map[string]any
	if err := json.Unmarshal(owner, &fields); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "pid", "ts"} {
		if _, ok := fields[k]; !ok {
			t.Errorf("the Go owner file has no %q field: %s", k, owner)
		}
	}
	if !strings.Contains(lock, "owner?.id") {
		t.Errorf("refresh-lock.ts no longer reads the owner id as owner?.id")
	}
}
