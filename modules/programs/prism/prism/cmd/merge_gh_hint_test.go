package cmd

// Regression tests for #3520: gh 2.105.0 prints a "hint:" block on stderr
// after `gh pr view`. prism must parse stdout only.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stubGhWithHint(t *testing.T, stdout string, exit int) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"cat <<'HINT' >&2\n" +
		"hint: if you need to wait for this pull request to be reviewed, merged, or closed before continuing, use this instead of polling:\n" +
		"    gh event listen --resource https://github.com/o/r/pull/1 --event com.github.pull_request.review.submitted.v1\n" +
		"HINT\n" +
		"cat <<'OUT'\n" + stdout + "\nOUT\n" +
		"exit " + string(rune('0'+exit)) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestProbeInitialState_IgnoresGhStderrHint(t *testing.T) {
	stubGhWithHint(t, `{"state":"MERGED","number":7,"title":"t","mergedAt":"2026-01-01T00:00:00Z"}`, 0)
	dec, err := probeInitialState(7)
	if err != nil {
		t.Fatalf("probeInitialState: %v", err)
	}
	if dec.Outcome != initialOutcomeAlreadyMerged {
		t.Fatalf("outcome = %v, want already merged", dec.Outcome)
	}
}

func TestProbeInitialState_NoJSONOnStdout(t *testing.T) {
	stubGhWithHint(t, ``, 0)
	_, err := probeInitialState(7)
	if err == nil || !strings.Contains(err.Error(), "parse gh pr view output") {
		t.Fatalf("err = %v, want parse error", err)
	}
}

func TestProbeInitialState_NonZeroKeepsStderr(t *testing.T) {
	stubGhWithHint(t, ``, 1)
	_, err := probeInitialState(7)
	if err == nil || !strings.Contains(err.Error(), "hint: if you need") {
		t.Fatalf("err = %v, want gh stderr text", err)
	}
}
