package sidecar

// host_api_safe_name_test.go — the host API refuses a session name or a
// branch name outside the safe set (issue #3308).
//
// tmux.nix puts a session name into shell commands. A sandboxed agent that
// gets a name with shell syntax into prism can run a command on the host.
// So each endpoint that takes a session name or a branch name from the
// caller refuses a name outside the safe set, with HTTP 400 and an error
// that names the character.
//
// Every sidecar here is built on sidecartest.NewIsolated and runs a shell
// stub in place of the prism binary, as in host_api_role_gate_test.go.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	safeNameCaller = "prism-test-safename@main"
	safeNameRepo   = "prism-test-safename"
	unsafeName     = "prism-test-safename@x$(id)"
)

// jsonBody marshals fields to a JSON object for a request body.
func jsonBody(t *testing.T, fields map[string]any) string {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return string(b)
}

// TestHostAPI_RefusesUnsafeSessionName lists each host-API endpoint that
// takes a session name from the caller. Each one must refuse an unsafe name
// with HTTP 400 and name the field and the character. The caller is a
// coordinator, so no role gate refuses the request first. If an endpoint
// loses its requireSafeName call, its case here fails.
func TestHostAPI_RefusesUnsafeSessionName(t *testing.T) {
	q := url.QueryEscape(unsafeName)
	cases := []struct {
		method string
		path   string
		body   map[string]any
		field  string
	}{
		{http.MethodGet, "/stats?view=detail&session=" + q, nil, "session"},
		{http.MethodGet, "/stats?view=compare&id=" + safeNameCaller + "&id=" + q, nil, "id"},
		{http.MethodGet, "/retro?train=" + q, nil, "train"},
		{http.MethodGet, "/checkin?session=" + q, nil, "session"},
		{http.MethodGet, "/checkin/review-summary?parent=" + q, nil, "parent"},
		{http.MethodGet, "/logs?session=" + q, nil, "session"},
		{http.MethodGet, "/sessions/status?session=" + q, nil, "session"},
		{http.MethodGet, "/audit?session=" + q, nil, "session"},
		{http.MethodGet, "/groups/monitor-log?round=1&session=" + q, nil, "session"},
		{http.MethodPost, "/prompt", map[string]any{"session": unsafeName, "prompt": "hi"}, "session"},
		{http.MethodPost, "/cleanup", map[string]any{"session": unsafeName, "yes": true}, "session"},
		{http.MethodPost, "/close", map[string]any{"session": unsafeName}, "session"},
		{http.MethodPost, "/switch", map[string]any{"session": unsafeName}, "session"},
		{http.MethodPost, "/event", map[string]any{"kind": "compaction", "session": unsafeName}, "session"},
		{http.MethodPost, "/feedback", map[string]any{"text": "note", "session": unsafeName}, "session"},
		{http.MethodPost, "/escalate", map[string]any{"prompt": "help", "to": unsafeName}, "to"},
		{http.MethodPost, "/escalate", map[string]any{"prompt": "help", "from": unsafeName}, "from"},
		{http.MethodPost, "/investigate", map[string]any{"prompt": "look", "from": unsafeName}, "from"},
		{http.MethodPost, "/set-model", map[string]any{"session": unsafeName, "model": "m"}, "session"},
		{http.MethodPost, "/apply-profile", map[string]any{"profile": "p", "scope": "session", "session": unsafeName}, "session"},
		{http.MethodPost, "/register-provider", map[string]any{"name": "n", "scope": "session", "session": unsafeName}, "session"},
		{http.MethodPost, "/register-provider-direct", map[string]any{"session": unsafeName, "name": "n"}, "session"},
		{http.MethodPost, "/set-active-tools", map[string]any{"session": unsafeName, "tools": []string{}}, "session"},
		{http.MethodPost, "/abort", map[string]any{"session": unsafeName}, "session"},
	}

	sc := newRoleGateSidecar(t, safeNameCaller, safeNameRepo, "coordinator", "coordinator", failingStub)
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path+" "+tc.field, func(t *testing.T) {
			body := ""
			if tc.body != nil {
				body = jsonBody(t, tc.body)
			}
			rr := doHostAPI(t, sc, tc.method, tc.path, body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %q, want 400", rr.Code, truncateForLog(rr.Body.String(), 300))
			}
			var resp map[string]string
			decodeJSONBody(t, rr, &resp)
			for _, want := range []string{"invalid " + tc.field + ":", `"$"`, `"("`, `")"`} {
				if !strings.Contains(resp["error"], want) {
					t.Errorf("error = %q, want it to hold %s", resp["error"], want)
				}
			}
		})
	}
}

// TestHostAPI_SafeSessionNamePasses makes sure that the check does not
// refuse a name in use: the same request with a safe name gets past the
// check.
func TestHostAPI_SafeSessionNamePasses(t *testing.T) {
	sc := newRoleGateSidecar(t, safeNameCaller, safeNameRepo, "coordinator", "coordinator", failingStub)
	for _, name := range []string{
		safeNameCaller,
		"prism-test-safename@feat~review-1-review-goal",
		"prism-test-safename@prismatic-bot--kube-prometheus-stack-89_x",
	} {
		rr := doHostAPI(t, sc, http.MethodGet, "/sessions/status?session="+url.QueryEscape(name), "")
		if rr.Code == http.StatusBadRequest {
			t.Errorf("/sessions/status?session=%s: status 400, body = %q, want the name accepted", name, rr.Body.String())
		}
	}
}

// TestHostAPI_Spawn_RefusesUnsafeBranch covers the /spawn half of the fix.
// The handler must refuse the branch before it runs the host-side prism
// spawn. The stub writes a marker file when it runs, so a marker means that
// a worktree and a session could exist.
func TestHostAPI_Spawn_RefusesUnsafeBranch(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "spawn-ran")
	stub := "#!/bin/sh\ntouch '" + marker + "'\nexit 1\n"
	sc := newRoleGateSidecar(t, safeNameCaller, safeNameRepo, "coordinator", "coordinator", stub)

	for branch, chars := range map[string][]string{
		"x$(id)":     {`"$"`, `"("`, `")"`},
		"x`id`":      {"\"`\""},
		"a;b":        {`";"`},
		"a b":        {`" "`},
		"feat~1":     {`"~"`},
		"x'y":        {`"'"`},
		"repo@other": {`"@"`},
	} {
		rr := doHostAPI(t, sc, http.MethodPost, "/spawn",
			jsonBody(t, map[string]any{"branch": branch, "prompt": "do the thing"}))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("branch %q: status = %d, body = %q, want 400", branch, rr.Code, rr.Body.String())
			continue
		}
		var resp map[string]string
		decodeJSONBody(t, rr, &resp)
		if !strings.HasPrefix(resp["error"], "invalid branch:") {
			t.Errorf("branch %q: error = %q, want the invalid-branch refusal", branch, resp["error"])
		}
		for _, c := range chars {
			if !strings.Contains(resp["error"], c) {
				t.Errorf("branch %q: error = %q, want it to name %s", branch, resp["error"], c)
			}
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the host-side prism spawn ran (marker %s: %v), want no run for a refused branch", marker, err)
	}

	// A branch in use today gets past the check and reaches the stub.
	rr := doHostAPI(t, sc, http.MethodPost, "/spawn",
		jsonBody(t, map[string]any{"branch": "prismatic-bot/kube-prometheus-stack-89.x", "prompt": "do the thing"}))
	if rr.Code == http.StatusBadRequest {
		t.Fatalf("safe branch: status 400, body = %q, want the branch accepted", rr.Body.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("safe branch: the stub did not run (%v), want the spawn to reach the host-side prism spawn", err)
	}
}
