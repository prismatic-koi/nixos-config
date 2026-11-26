package mergequeue

// Regression test for #3520: the real execGH path must not mix the gh 2.105.0
// stderr hint into the JSON that fetchPRInfo parses.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFetchPRInfo_IgnoresGhStderrHint(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"echo 'hint: if you need to wait for this pull request, use gh event listen' >&2\n" +
		`echo '{"state":"OPEN","mergedAt":null,"mergeStateStatus":"CLEAN","statusCheckRollup":[],"reviewDecision":""}'` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	w := &Watcher{repo: "o/r"}
	info, err := w.fetchPRInfo(context.Background(), 1)
	if err != nil {
		t.Fatalf("fetchPRInfo: %v", err)
	}
	if info.State != "OPEN" {
		t.Fatalf("state = %q", info.State)
	}
}
