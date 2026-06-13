package cmd

// wait_probe.go — sandbox-aware probe sources for the --wait poll loops
// (issue #1500).
//
// Background. The merge / review / spawn wait loops poll prism DB rows for a
// terminal state. When prism runs on the host, openDB() is the right path —
// the worker writes to and reads from the same DB the sidecar's watchers /
// monitors write to. When prism runs INSIDE a sandbox (PRISM_HOST_API set),
// openDB() opens a shadow tmpfs DB the host watcher never writes to, so the
// poll never observes a terminal and --wait silently hangs until timeout.
//
// review-code on PR #1533 caught this for `prism review --wait` and
// `prism spawn --wait` (the proxy paths returned before reaching the wait
// block); the merge case had the same latent bug. The fix is the abstraction
// in this file: each wait loop talks to a "probe source" that hides whether
// the underlying lookup is a direct DB call (host) or an HTTP GET to the
// sidecar's /merges/by-pr, /sessions/status, or /groups/poll endpoints
// (sandbox).
//
// A single newWaitProbe() resolves the right source from
// sandboxenv.HostAPISocket() — host vs sandbox — so the call sites in
// merge.go / review_wait.go / spawn_wait.go just call probe.Merge(pr) etc.

import (
	"fmt"
	"strings"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/sandboxenv"
)

// waitProbe is the interface the wait loops use to read terminal state.
// Implementations may close over a *db.DB (host path) or a host-API URL
// (sandbox path); the wait loops do not care which.
type waitProbe interface {
	// Merge returns the pending_merges row for pr, or (nil, nil) when no
	// row exists. err is reserved for transient failures (the wait loop
	// retries on err).
	Merge(pr int) (*db.PendingMerge, error)

	// SessionStatus returns the agent_status row for sessionName, or
	// (nil, nil) when no row exists.
	SessionStatus(sessionName string) (*db.Status, error)

	// GroupPoll returns the per-group completion + members + results in a
	// single round-trip. completed=true means every member has reached a
	// terminal state. members lists every row in the group (including
	// rows whose ended_at is set, which GroupResults intentionally drops);
	// results carries the verdict-aggregation data from GroupResults.
	GroupPoll(groupID string) (completed bool, members []db.Status, results map[string]db.GroupMemberResult, err error)

	// Close releases any underlying handle. host probes hold a *db.DB;
	// sandbox probes hold no resources.
	Close()
}

// newWaitProbe returns the appropriate probe for the current process. When
// PRISM_HOST_API is set we route reads through the sidecar; otherwise we
// open the host's prism.db read-write (the wait loops only read, but
// openDB returns a writable handle and that is fine). The caller must
// Close() the returned probe when done.
func newWaitProbe() (waitProbe, error) {
	if apiURL := sandboxenv.HostAPISocket(); apiURL != "" {
		return &proxyWaitProbe{apiURL: apiURL}, nil
	}
	d, err := openDB()
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	return &dbWaitProbe{d: d}, nil
}

// dbWaitProbe is the host-path implementation: direct DB reads.
type dbWaitProbe struct{ d *db.DB }

func (p *dbWaitProbe) Merge(pr int) (*db.PendingMerge, error) {
	return p.d.PendingMergeByPR(pr)
}

func (p *dbWaitProbe) SessionStatus(sessionName string) (*db.Status, error) {
	return p.d.CurrentStatus(sessionName)
}

func (p *dbWaitProbe) GroupPoll(groupID string) (bool, []db.Status, map[string]db.GroupMemberResult, error) {
	completed, gErr := p.d.GroupCompleted(groupID)
	if gErr != nil {
		return false, nil, nil, gErr
	}
	members, mErr := p.d.GroupMembersForGroup(groupID)
	if mErr != nil {
		return false, nil, nil, mErr
	}
	results, rErr := p.d.GroupResults(groupID)
	if rErr != nil {
		return false, nil, nil, rErr
	}
	return completed, members, results, nil
}

func (p *dbWaitProbe) Close() {
	if p.d != nil {
		p.d.Close()
	}
}

// proxyWaitProbe is the sandbox-path implementation: HTTP GETs to the
// sidecar's read-only wait-probe endpoints.
type proxyWaitProbe struct{ apiURL string }

func (p *proxyWaitProbe) Merge(pr int) (*db.PendingMerge, error) {
	var row db.PendingMerge
	params := map[string]string{"pr": fmt.Sprintf("%d", pr)}
	err := proxyGetFromHostAPI(p.apiURL, "/merges/by-pr", params, &row)
	if err != nil {
		// not-found surfaces as a 404; the proxy helper wraps it into an
		// error string. Distinguish "not found" (return nil, nil — same
		// shape as the direct DB path) from other errors so the wait loop
		// can keep polling.
		if isProxyNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &row, nil
}

func (p *proxyWaitProbe) SessionStatus(sessionName string) (*db.Status, error) {
	var st db.Status
	params := map[string]string{"session": sessionName}
	err := proxyGetFromHostAPI(p.apiURL, "/sessions/status", params, &st)
	if err != nil {
		if isProxyNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &st, nil
}

// proxyGroupPollResp matches the JSON shape emitted by /groups/poll. It is
// kept as a private type because the wait loops always destructure it into
// separate return values; exposing the wrapper would add ceremony.
type proxyGroupPollResp struct {
	Completed bool                            `json:"completed"`
	Members   []db.Status                     `json:"members"`
	Results   map[string]db.GroupMemberResult `json:"results"`
}

func (p *proxyWaitProbe) GroupPoll(groupID string) (bool, []db.Status, map[string]db.GroupMemberResult, error) {
	var resp proxyGroupPollResp
	params := map[string]string{"group_id": groupID}
	if err := proxyGetFromHostAPI(p.apiURL, "/groups/poll", params, &resp); err != nil {
		return false, nil, nil, err
	}
	return resp.Completed, resp.Members, resp.Results, nil
}

func (p *proxyWaitProbe) Close() {}

// isProxyNotFound returns true when err is a host-API 404 surfaced by
// proxyGetFromHostAPI. The helper wraps non-2xx responses into an error
// containing the body's `error` field, so we match on the substring used
// uniformly across the new wait endpoints.
func isProxyNotFound(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "not found")
}
