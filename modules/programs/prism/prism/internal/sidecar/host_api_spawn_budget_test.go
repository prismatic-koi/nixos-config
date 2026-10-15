package sidecar

// The host-API /spawn handler relays the spawn budget line (issue #3177).
// The host-side `prism spawn` prints the line to stderr for a worker spawn.
// The sandboxed caller reads only the JSON response, so the handler must
// copy the line into "spawn_budget".

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/spawnbudget"
)

const testBudgetLine = spawnbudget.LinePrefix + " 115 of 120 worker spawns in the last 7 days (95%, all hosts). WARNING: 95% of the limit or more. Finish the current work and stop for the day."

// writeStubBinaryWithStderr writes an executable stub prism that prints the
// spawn "created" line to stdout and stderrLine to stderr.
func writeStubBinaryWithStderr(t *testing.T, dir, stderrLine string) string {
	t.Helper()
	path := filepath.Join(dir, "prism")
	stub := "#!/bin/sh\n"
	if stderrLine != "" {
		stub += "echo '" + stderrLine + "' >&2\n"
	}
	stub += "echo 'session \"test-repo@feature\" created'\n"
	if err := os.WriteFile(path, []byte(stub), 0o755); err != nil {
		t.Fatalf("write stub binary: %v", err)
	}
	return path
}

func TestHostAPI_Spawn_RelaysSpawnBudgetLine(t *testing.T) {
	cases := []struct {
		name       string
		stderrLine string
		body       string
		want       string
	}{
		{"worker spawn", testBudgetLine, `{"branch":"feature","prompt":"go"}`, testBudgetLine},
		{"abtest pair", testBudgetLine, `{"branch":"feature","prompt":"go","abtest":["light","heavy"]}`, testBudgetLine},
		{"no budget line", "", `{"branch":"feature","prompt":"go"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin := writeStubBinaryWithStderr(t, t.TempDir(), tc.stderrLine)
			sc, _ := newSpawnSidecarForBinaryStaleness(t, bin)
			putOnPath(t, bin)

			rr := doHostAPI(t, sc, http.MethodPost, "/spawn", tc.body)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
			}
			var resp map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal response: %v; body = %s", err, rr.Body.String())
			}
			got, present := resp["spawn_budget"]
			if tc.want == "" {
				if present {
					t.Errorf("spawn_budget = %v, want the field absent", got)
				}
				return
			}
			if got != tc.want {
				t.Errorf("spawn_budget = %v, want %q", got, tc.want)
			}
		})
	}
}

func TestParseSpawnBudgetLine(t *testing.T) {
	out := "warning: something\n  " + testBudgetLine + "  \nsession \"r@b\" created\n"
	if got := parseSpawnBudgetLine(out); got != testBudgetLine {
		t.Errorf("parseSpawnBudgetLine = %q, want %q", got, testBudgetLine)
	}
	if got := parseSpawnBudgetLine("session \"r@b\" created\n"); got != "" {
		t.Errorf("parseSpawnBudgetLine without a budget line = %q, want empty", got)
	}
}
