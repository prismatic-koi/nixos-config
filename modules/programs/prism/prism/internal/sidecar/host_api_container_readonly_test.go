package sidecar

import (
	"net/http"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/prismcontainer"
	"github.com/prismatic-koi/prism/internal/prismcontainer/prismcontainertest"
)

// TestHostAPI_ContainerMountRW_ReadOnlyWorktreeRefused: the sidecar route
// gives Config.WorktreeReadOnly to prismcontainer, so a read-only session
// cannot mount its worktree read-write (#3079).
func TestHostAPI_ContainerMountRW_ReadOnlyWorktreeRefused(t *testing.T) {
	for _, path := range []string{"/container/run", "/container/start"} {
		t.Run(path, func(t *testing.T) {
			fake := &prismcontainertest.Fake{ImagePresent: true}
			sc := newContainerTestSidecar(t, "review-code", fake)
			sc.cfg.WorktreeReadOnly = true

			rr := doHostAPI(t, sc, http.MethodPost, path, `{"image":"alpine","mount":"rw"}`)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
			}
			var res prismcontainer.RunResult
			decodeJSONBody(t, rr, &res)
			if res.ExitCode != prismcontainer.ExitRefused || !strings.Contains(res.Message, "read-only") {
				t.Errorf("result = %+v, want the read-only worktree refusal", res)
			}
			if calls := fake.Calls(); len(calls) != 0 {
				t.Errorf("podman ran: %q", calls)
			}
		})
	}
}
