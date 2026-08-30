package cmd

// Regression test: `prism pr <n>` in container mode (PRISM_HOST_API set) must
// forward the raw PR number as "pr" to the host-API /spawn endpoint, NOT
// resolve it to a (possibly sanitised) branch name client-side. Resolving
// client-side forks a new branch from the default branch whenever the real PR
// head ref contains a slash, because the host-side resolveBranch runs the
// forwarded branch name through git.SanitiseBranch ("/" -> "-") and finds no
// matching local or origin branch.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPrCmd_ContainerMode_ForwardsPRNotBranch verifies that prCmd.RunE, when
// PRISM_HOST_API is set, POSTs {"pr": "<n>"} to /spawn and does NOT include a
// "branch" field — i.e. it never resolves the PR to a branch name locally.
func TestPrCmd_ContainerMode_ForwardsPRNotBranch(t *testing.T) {
	type spawnReq struct {
		PR     string `json:"pr"`
		Branch string `json:"branch"`
		Repo   string `json:"repo"`
	}

	reqCh := make(chan spawnReq, 1)

	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/spawn" {
			http.Error(w, `{"error":"wrong path"}`, http.StatusBadRequest)
			return
		}
		var raw map[string]any
		_ = json.NewDecoder(r.Body).Decode(&raw)
		b, _ := json.Marshal(raw)
		var req spawnReq
		_ = json.Unmarshal(b, &req)
		if _, ok := raw["branch"]; !ok {
			req.Branch = "__absent__"
		}
		reqCh <- req
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"session_name":"nixos-config@feat-lago-staging-db"}`))
	})

	t.Setenv("PRISM_HOST_API", srv.apiURL())

	// Fake a raw bare git dir (container layout: bare repo mounted directly,
	// see git.IsRawBareGitDir) so resolveBareRoot succeeds without invoking
	// real git or tmux.
	bareDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(bareDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}
	if err := os.Mkdir(filepath.Join(bareDir, "objects"), 0o755); err != nil {
		t.Fatalf("mkdir objects: %v", err)
	}
	t.Setenv("PRISM_BARE_ROOT", bareDir)

	_ = prCmd.Flags().Set("prompt", "review this PR")
	t.Cleanup(func() { _ = prCmd.Flags().Set("prompt", "") })

	prCmd.SetArgs([]string{"386"})
	if err := prCmd.RunE(prCmd, []string{"386"}); err != nil {
		t.Fatalf("prCmd.RunE: %v", err)
	}

	select {
	case req := <-reqCh:
		if req.PR != "386" {
			t.Errorf("pr = %q, want %q", req.PR, "386")
		}
		if req.Branch != "__absent__" {
			t.Errorf("branch = %q, want absent from request body \u2014 prism pr must not resolve the PR to a branch client-side (issue #2432)", req.Branch)
		}
		if req.Repo != filepath.Base(bareDir) {
			t.Errorf("repo = %q, want %q", req.Repo, filepath.Base(bareDir))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for request")
	}
}
