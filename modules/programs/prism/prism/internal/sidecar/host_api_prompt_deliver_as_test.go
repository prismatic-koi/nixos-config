package sidecar

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// TestHostAPI_Prompt_CrossSession_DeliverAs checks that the cross-session
// /prompt handler passes --deliver-as to the host-side `prism prompt`
// command, and rejects invalid values before any command runs (#3487).
func TestHostAPI_Prompt_CrossSession_DeliverAs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stubs are POSIX-only")
	}
	cases := []struct {
		name     string
		field    string
		wantCode int
		wantFlag string // empty: the stub must not run
	}{
		{"followUp", `,"deliver_as":"followUp"`, http.StatusOK, "followUp"},
		{"nextTurn", `,"deliver_as":"nextTurn"`, http.StatusOK, "nextTurn"},
		{"steer", `,"deliver_as":"steer"`, http.StatusOK, "steer"},
		{"default", ``, http.StatusOK, "nextTurn"},
		{"invalid", `,"deliver_as":"bogus"`, http.StatusBadRequest, ""},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			argsFile := filepath.Join(dir, "args")
			stub := filepath.Join(dir, "prism-stub")
			script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsFile + "'\n" +
				"echo '{\"delivered_to\":\"target-repo@worker\"}'\n"
			if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
				t.Fatalf("write stub: %v", err)
			}
			bus := sidecartest.NewIsolated(t, "")
			sc := newCrossSessionSidecar(t, "prism-test@coord-deliver-as-"+string(rune('a'+i)),
				"target-repo", "coordinator", stub, bus.DB)
			seedTargetSession(t, bus.DB, "target-repo@worker", "target-repo")

			body := `{"session":"target-repo@worker","prompt":"hello"` + tc.field + `}`
			rr := doHostAPI(t, sc, http.MethodPost, "/prompt", body)
			if rr.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d; body=%s", rr.Code, tc.wantCode, rr.Body.String())
			}
			raw, err := os.ReadFile(argsFile)
			if tc.wantFlag == "" {
				if err == nil {
					t.Fatalf("host command ran for invalid deliver_as: %q", raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("read args: %v", err)
			}
			args := strings.Split(strings.TrimSpace(string(raw)), "\n")
			found := false
			for j := 0; j+1 < len(args); j++ {
				if args[j] == "--deliver-as" && args[j+1] == tc.wantFlag {
					found = true
				}
			}
			if !found {
				t.Errorf("host args %q lack --deliver-as %s", args, tc.wantFlag)
			}
		})
	}
}
