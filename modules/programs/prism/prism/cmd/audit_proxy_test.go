package cmd

// Tests for the PRISM_HOST_API proxy branch in `prism audit`.
//
// $XDG_STATE_HOME/prism holds prism.db and is deliberately never bound into a
// sandbox, so a sandboxed caller cannot open it. Without a proxy branch,
// `prism audit` is unreadable by the seat that writes the audit trail.
//
// The assertions here are the CLI half:
//
//   - the proxy branch is taken when PRISM_HOST_API is set, and the four
//     filters (--days, --pattern, --limit, session argument) reach the host;
//   - the rendered output is byte-identical to the direct-DB path for the
//     same inputs, in table mode and in --json mode;
//   - the direct-DB path still runs when PRISM_HOST_API is unset;
//   - a host-side refusal (403) surfaces as an error, not as an empty trail.
//
// The host-side handler is pinned separately in
// internal/sidecar/host_api_audit_test.go. The server here delegates to the
// same db helper that handler calls, so the round-trip is faithful without
// re-implementing the query. This mirrors startStatsProxyDBServer in
// stats_proxy_test.go.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
)

// newAuditFlagsCmd returns a cobra.Command carrying the same flags the real
// audit command registers, isolated from the global command's flag state so
// each test can set flags without cross-test pollution.
func newAuditFlagsCmd(t *testing.T) *cobra.Command {
	t.Helper()
	c := &cobra.Command{Use: "audit"}
	c.Flags().Int("days", 0, "")
	c.Flags().String("pattern", "", "")
	c.Flags().Int("limit", 0, "")
	c.Flags().Bool("json", false, "")
	return c
}

// setAuditFlags applies a name→value map to the command's flags.
func setAuditFlags(t *testing.T, c *cobra.Command, flags map[string]string) {
	t.Helper()
	for name, value := range flags {
		if err := c.Flags().Set(name, value); err != nil {
			t.Fatalf("set --%s=%s: %v", name, value, err)
		}
	}
}

// writeAuditTestEvent seeds one agent_events row of an arbitrary type.
func writeAuditTestEvent(t *testing.T, d *db.DB, session, typ, payloadJSON string, at time.Time) {
	t.Helper()
	if err := d.WriteEvent(db.Event{
		ID:          uuid.New().String(),
		SessionName: session,
		Repo:        "testrepo",
		Worktree:    "/code/testrepo/main",
		Type:        typ,
		Payload:     payloadJSON,
		CreatedAt:   at,
	}); err != nil {
		t.Fatalf("WriteEvent(%s/%s): %v", session, typ, err)
	}
}

// seedAuditProxyRows seeds a mix of audit and non-audit rows spread over
// several days, so --days, --pattern, --limit, and the session argument each
// have something to discriminate on.
func seedAuditProxyRows(t *testing.T, d *db.DB) {
	t.Helper()
	now := time.Now()

	writeAuditTestEvent(t, d, "testrepo@main", "audit",
		`{"tool":"bash","command":"gh pr merge 2618 --squash","sessionName":"testrepo@main"}`,
		now.Add(-1*time.Hour))
	writeAuditTestEvent(t, d, "testrepo@main", "audit",
		`{"tool":"bash","command":"git push --force","sessionName":"testrepo@main"}`,
		now.Add(-2*time.Hour))
	writeAuditTestEvent(t, d, "testrepo@feature-x", "audit",
		`{"tool":"bash","command":"nix build .#prism","sessionName":"testrepo@feature-x"}`,
		now.Add(-3*time.Hour))
	// Older than a 2-day window, so --days 2 must exclude it.
	writeAuditTestEvent(t, d, "testrepo@feature-x", "audit",
		`{"tool":"bash","command":"gh pr merge 1000 --squash","sessionName":"testrepo@feature-x"}`,
		now.Add(-96*time.Hour))

	// Non-audit rows: must never appear in `prism audit` output on either route.
	writeAuditTestEvent(t, d, "testrepo@main", "tool_call",
		`{"command":"cat /etc/passwd","tool":"bash"}`, now.Add(-30*time.Minute))
	writeAuditTestEvent(t, d, "testrepo@feature-x", "assistant_turn",
		`{"command":"private reasoning","text":"secret"}`, now.Add(-45*time.Minute))
}

// startAuditProxyServer stands up a Unix-socket GET /audit server that
// delegates to db.QueryAuditEvents — the same helper the real host-side
// handler calls. It records the raw query string of the last request so a
// test can assert the filters that crossed the wire.
type auditProxyServer struct {
	capturedPath  string
	capturedQuery string
	requests      int
}

func startAuditProxyServer(t *testing.T, d *db.DB) (*auditProxyServer, string) {
	t.Helper()
	srv := &auditProxyServer{}
	mock := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		srv.capturedPath = r.URL.Path
		srv.capturedQuery = r.URL.RawQuery
		srv.requests++

		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "method not allowed"})
			return
		}
		q := r.URL.Query()
		sinceMs, _ := strconv.ParseInt(q.Get("since"), 10, 64)
		limit, _ := strconv.Atoi(q.Get("limit"))
		events, err := d.QueryAuditEvents(q.Get("session"), sinceMs, q.Get("pattern"), limit)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		if events == nil {
			events = []db.Event{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"events": events})
	})
	return srv, mock.apiURL()
}

// renderAuditDirect runs `prism audit` against the direct-DB path.
//
// The direct route is gated coordinator-only, so the caller
// session must resolve to a coordinator. auditProxyDirectCaller is seeded as
// one on first use, and PRISM_SESSION_NAME points every direct-route test in
// this file at it.
func renderAuditDirect(t *testing.T, flags map[string]string, args ...string) string {
	t.Helper()
	t.Setenv("PRISM_HOST_API", "")
	seedAuditProxyDirectCaller(t)
	t.Setenv("PRISM_SESSION_NAME", auditProxyDirectCaller)
	t.Setenv("TMUX", "")
	c := newAuditFlagsCmd(t)
	setAuditFlags(t, c, flags)
	return captureStdout(t, func() {
		if err := runAudit(c, args); err != nil {
			t.Fatalf("runAudit (direct): %v", err)
		}
	})
}

// auditProxyDirectCaller is the coordinator session the direct-route tests in
// this file authenticate as.
const auditProxyDirectCaller = "prism-test-audit-proxy@main"

// seedAuditProxyDirectCaller inserts a coordinator agent_status row for
// auditProxyDirectCaller against the DB the test's openDB() call resolves to
// (set by openStatsTestDB via SetTestDBPath). Idempotent: an upsert, so
// calling it more than once across sub-tests sharing one DB is safe.
func seedAuditProxyDirectCaller(t *testing.T) {
	t.Helper()
	d, err := openDB()
	if err != nil {
		t.Fatalf("seedAuditProxyDirectCaller: openDB: %v", err)
	}
	defer d.Close()
	if err := d.UpsertStatusSeedRootAgentName(
		auditProxyDirectCaller, "prism-test-audit-proxy", "/worktree/main", "idle",
		nil, nil, "coordinator", "", "host",
	); err != nil {
		t.Fatalf("seedAuditProxyDirectCaller: UpsertStatusSeedRootAgentName: %v", err)
	}
}

// renderAuditProxy runs `prism audit` against the proxy path.
func renderAuditProxy(t *testing.T, apiURL string, flags map[string]string, args ...string) string {
	t.Helper()
	t.Setenv("PRISM_HOST_API", apiURL)
	c := newAuditFlagsCmd(t)
	setAuditFlags(t, c, flags)
	return captureStdout(t, func() {
		if err := runAudit(c, args); err != nil {
			t.Fatalf("runAudit (proxy): %v", err)
		}
	})
}

// ── the proxy branch is taken, and carries the filters ───────────────────────

// TestRunAudit_Proxy_SendsFilters verifies that --days, --pattern, --limit and
// the session argument all reach GET /audit as query parameters. Before the
// fix there was no request at all: runAudit called openDB unconditionally.
func TestRunAudit_Proxy_SendsFilters(t *testing.T) {
	d := openStatsTestDB(t)
	seedAuditProxyRows(t, d)
	srv, apiURL := startAuditProxyServer(t, d)

	before := time.Now().Add(-3 * 24 * time.Hour).UnixMilli()
	renderAuditProxy(t, apiURL,
		map[string]string{"days": "3", "pattern": "gh pr merge", "limit": "5"},
		"testrepo@main")
	after := time.Now().Add(-3 * 24 * time.Hour).UnixMilli()

	if srv.requests != 1 {
		t.Fatalf("server saw %d request(s), want 1", srv.requests)
	}
	if srv.capturedPath != "/audit" {
		t.Errorf("path = %q, want /audit", srv.capturedPath)
	}

	q, err := url.ParseQuery(srv.capturedQuery)
	if err != nil {
		t.Fatalf("parse captured query %q: %v", srv.capturedQuery, err)
	}
	if q.Get("session") != "testrepo@main" {
		t.Errorf("session = %q, want testrepo@main (query = %q)", q.Get("session"), srv.capturedQuery)
	}
	if q.Get("pattern") != "gh pr merge" {
		t.Errorf("pattern = %q, want %q (query = %q)", q.Get("pattern"), "gh pr merge", srv.capturedQuery)
	}
	if q.Get("limit") != "5" {
		t.Errorf("limit = %q, want 5 (query = %q)", q.Get("limit"), srv.capturedQuery)
	}
	// --days 3 is converted to an absolute `since` in Unix ms by the CLI, so
	// assert it lands inside the window bounded by the two clock reads above.
	since, convErr := strconv.ParseInt(q.Get("since"), 10, 64)
	if convErr != nil {
		t.Fatalf("since = %q, want an integer (query = %q)", q.Get("since"), srv.capturedQuery)
	}
	if since < before || since > after {
		t.Errorf("since = %d, want it inside [%d, %d] (3 days before now)", since, before, after)
	}
}

// TestRunAudit_Proxy_OmitsInactiveFilters verifies that a filter the user did
// not set is not sent at all. An empty `pattern=` would still be an empty
// filter host-side, but sending it invites divergence if the host-side
// default ever changes.
func TestRunAudit_Proxy_OmitsInactiveFilters(t *testing.T) {
	d := openStatsTestDB(t)
	seedAuditProxyRows(t, d)
	srv, apiURL := startAuditProxyServer(t, d)

	renderAuditProxy(t, apiURL, nil)

	if srv.capturedQuery != "" {
		t.Errorf("query = %q, want empty when no flags and no session argument", srv.capturedQuery)
	}
}

// TestRunAudit_HostPath_SendsNoRequest verifies the direct-DB path is
// untouched: with PRISM_HOST_API unset, no host-API request is made.
func TestRunAudit_HostPath_SendsNoRequest(t *testing.T) {
	d := openStatsTestDB(t)
	seedAuditProxyRows(t, d)
	srv, _ := startAuditProxyServer(t, d)

	out := renderAuditDirect(t, nil)

	if srv.requests != 0 {
		t.Errorf("server saw %d request(s) on the direct path, want 0", srv.requests)
	}
	if out == "" {
		t.Error("direct path produced no output")
	}
}

// ── output parity between the two routes ─────────────────────────────────────

// TestRunAudit_Proxy_MatchesHostOutput is the parity AC: for the same inputs,
// the proxied path returns the same rows the host-mode path returns, and the
// CLI renders them identically. Rendering lives on the CLI side precisely so
// this holds.
func TestRunAudit_Proxy_MatchesHostOutput(t *testing.T) {
	d := openStatsTestDB(t)
	seedAuditProxyRows(t, d)
	srv, apiURL := startAuditProxyServer(t, d)

	cases := []struct {
		name  string
		flags map[string]string
		args  []string
	}{
		{"no filters", nil, nil},
		{"session argument", nil, []string{"testrepo@main"}},
		{"session with no rows", nil, []string{"testrepo@absent"}},
		{"days", map[string]string{"days": "2"}, nil},
		{"days zero means no window", map[string]string{"days": "0"}, nil},
		{"pattern", map[string]string{"pattern": "gh pr merge"}, nil},
		{"pattern is case-insensitive", map[string]string{"pattern": "GH PR MERGE"}, nil},
		{"pattern matching nothing", map[string]string{"pattern": "zzz-no-match"}, nil},
		{"limit", map[string]string{"limit": "2"}, nil},
		{"limit larger than the row count", map[string]string{"limit": "500"}, nil},
		{"every filter at once",
			map[string]string{"days": "7", "pattern": "merge", "limit": "3"},
			[]string{"testrepo@feature-x"}},
		{"json", map[string]string{"json": "true"}, nil},
		{"json with filters",
			map[string]string{"json": "true", "days": "2", "pattern": "git push", "limit": "10"},
			nil},
		{"json with no results",
			map[string]string{"json": "true", "pattern": "zzz-no-match"}, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			direct := renderAuditDirect(t, tc.flags, tc.args...)

			// The proxy leg must really cross the socket. Without this
			// count the comparison would pass vacuously: testDBPath is set,
			// so a missing proxy branch would fall through to the same
			// local DB and render the same bytes.
			before := srv.requests
			proxied := renderAuditProxy(t, apiURL, tc.flags, tc.args...)
			if srv.requests != before+1 {
				t.Fatalf("server saw %d request(s) for the proxy leg, want 1",
					srv.requests-before)
			}

			if direct != proxied {
				t.Errorf("proxy output differs from the direct-DB path\ndirect:\n%s\nproxy:\n%s",
					direct, proxied)
			}
		})
	}
}

// TestRunAudit_Proxy_ReturnsOnlyAuditRows guards the CLI half of the type
// filter: the seeded tool_call and assistant_turn payloads must not reach the
// rendered output on either route.
func TestRunAudit_Proxy_ReturnsOnlyAuditRows(t *testing.T) {
	d := openStatsTestDB(t)
	seedAuditProxyRows(t, d)
	_, apiURL := startAuditProxyServer(t, d)

	flags := map[string]string{"json": "true", "limit": "500"}
	routes := []struct {
		name string
		out  string
	}{
		{"direct", renderAuditDirect(t, flags)},
		{"proxy", renderAuditProxy(t, apiURL, flags)},
	}

	for _, route := range routes {
		// Guard against a vacuous pass: an empty result contains no leak
		// either, so confirm the route returned rows at all.
		if !strings.Contains(route.out, "gh pr merge 2618 --squash") {
			t.Fatalf("%s route returned no audit rows; the leak assertions would be vacuous:\n%s",
				route.name, route.out)
		}
		for _, leak := range []string{"cat /etc/passwd", "private reasoning"} {
			if strings.Contains(route.out, leak) {
				t.Errorf("%s route leaked a non-audit payload containing %q:\n%s",
					route.name, leak, route.out)
			}
		}
	}
}

// ── error surfacing ──────────────────────────────────────────────────────────

// TestRunAudit_Proxy_ForbiddenSurfacesError verifies that a host-side refusal
// becomes a CLI error rather than a silently empty audit trail. The endpoint
// is coordinator-only, so a worker gets 403.
func TestRunAudit_Proxy_ForbiddenSurfacesError(t *testing.T) {
	mock := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "workers cannot perform audit"})
	})
	t.Setenv("PRISM_HOST_API", mock.apiURL())

	c := newAuditFlagsCmd(t)
	err := runAudit(c, nil)
	if err == nil {
		t.Fatal("runAudit returned nil on a 403; want an error")
	}
	if !strings.Contains(err.Error(), "workers cannot perform audit") {
		t.Errorf("error = %q, want it to carry the host-side message", err.Error())
	}
	if !strings.HasPrefix(err.Error(), "audit:") {
		t.Errorf("error = %q, want it prefixed with the command name", err.Error())
	}
}

// TestRunAudit_Proxy_UnreachableSocketSurfacesError verifies that a dead
// socket is a clear error, not a silent fallback to the local DB — which
// inside a sandbox would fail anyway, with a far more confusing message.
func TestRunAudit_Proxy_UnreachableSocketSurfacesError(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "unix:///nonexistent/prism-audit-test.sock")

	c := newAuditFlagsCmd(t)
	err := runAudit(c, nil)
	if err == nil {
		t.Fatal("runAudit returned nil against a dead socket; want an error")
	}
	if !strings.HasPrefix(err.Error(), "audit:") {
		t.Errorf("error = %q, want it prefixed with the command name", err.Error())
	}
}
