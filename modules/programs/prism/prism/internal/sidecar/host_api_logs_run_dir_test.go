package sidecar

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	prismsession "github.com/prismatic-koi/prism/internal/session"
)

// TestHostAPI_Logs_AgentRunLogSymlinkRefused: the agent-run log is in the
// run dir, which the sandbox of the target can write. A symlink there to a
// file of the user, for example a credential, must not make /logs read and
// return that file, in each read mode (#3373). A plain agent-run log is
// still served.
func TestHostAPI_Logs_AgentRunLogSymlinkRefused(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const target = "myrepo@feature"
	logPath, err := prismsession.AgentRunLogPath(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("agent line\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	d := openTestDB(t)
	// A finished target makes the follow mode return after one read, so a
	// read through the symlink fails the test instead of a wait.
	if err := d.UpsertStatus(target, "myrepo", "/wt", "finished", nil, nil); err != nil {
		t.Fatal(err)
	}
	sc := newSidecarWithRole(t, "myrepo@main", "myrepo", "coordinator", d)
	if rr := doHostAPI(t, sc, http.MethodGet, "/logs?session="+target+"&source=agent-run", ""); rr.Code != http.StatusOK || rr.Body.String() != "agent line\n" {
		t.Fatalf("plain agent-run log: status %d, body %q; want 200 and the log", rr.Code, rr.Body.String())
	}

	const secret = "ghp_secret_token_value"
	secretFile := filepath.Join(t.TempDir(), "github_token")
	if err := os.WriteFile(secretFile, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secretFile, logPath); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"", "&tail=2", "&follow=true"} {
		rr := doHostAPI(t, sc, http.MethodGet, "/logs?session="+target+"&source=agent-run"+query, "")
		if rr.Code == http.StatusOK || strings.Contains(rr.Body.String(), secret) {
			t.Errorf("/logs%s with a symlinked agent-run log: status %d, body %q; want a refusal without the target content", query, rr.Code, rr.Body.String())
		}
	}
}
