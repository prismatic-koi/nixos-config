package cmd

// Tests for the PRISM_HOST_API proxy dispatch in prism stats (#1463).
//
// Each test spins up a real HTTP server bound to a Unix socket, sets
// PRISM_HOST_API, and verifies that:
//   - the correct GET request (path, query params) is sent to the host sidecar
//   - the rendered output is produced from the proxy response (rendering on CLI side)
//   - --json emits the raw response JSON
//   - when PRISM_HOST_API is unset, the proxy is a no-op (existing tests cover direct path)

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
)

// fakeStatsServer is a minimal mock for the /stats endpoint.
type fakeStatsServer struct {
	capturedPath  string
	capturedQuery string
	response      []byte
	statusCode    int
}

func (s *fakeStatsServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		s.capturedPath = r.URL.Path
		s.capturedQuery = r.URL.RawQuery

		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"want GET"}`, http.StatusMethodNotAllowed)
			return
		}

		status := s.statusCode
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(s.response)
	})
	return mux
}

// startFakeStatsServer starts the fake /stats server bound to a Unix socket.
func startFakeStatsServer(t *testing.T, resp []byte) (*fakeStatsServer, string) {
	t.Helper()
	srv := &fakeStatsServer{response: resp}
	mock := newMockUnixServer(t, srv.handler().ServeHTTP)
	return srv, mock.apiURL()
}

// ── proxyStats helper tests ───────────────────────────────────────────────────

// TestProxyStats_SendsCorrectQueryParams verifies that proxyStats sends a GET
// /stats request with the correct view and session query params.
func TestProxyStats_SendsCorrectQueryParams(t *testing.T) {
	respBody := `{"events":[]}`
	srv, apiURL := startFakeStatsServer(t, []byte(respBody))

	raw, err := proxyStats(apiURL, "doomloops", "myrepo@main", 7, "", 0)
	if err != nil {
		t.Fatalf("proxyStats: %v", err)
	}
	if srv.capturedPath != "/stats" {
		t.Errorf("path = %q, want /stats", srv.capturedPath)
	}
	if !strings.Contains(srv.capturedQuery, "view=doomloops") {
		t.Errorf("query %q does not contain view=doomloops", srv.capturedQuery)
	}
	if !strings.Contains(srv.capturedQuery, "session=myrepo%40main") {
		t.Errorf("query %q does not contain session=myrepo%%40main", srv.capturedQuery)
	}
	if !strings.Contains(srv.capturedQuery, "days=7") {
		t.Errorf("query %q does not contain days=7", srv.capturedQuery)
	}
	if len(raw) == 0 {
		t.Error("expected non-empty raw response")
	}
}

// TestProxyStats_SummaryView verifies the view=summary query param is sent.
func TestProxyStats_SummaryView(t *testing.T) {
	respBody := `{"sessions":[]}`
	srv, apiURL := startFakeStatsServer(t, []byte(respBody))

	_, err := proxyStats(apiURL, "summary", "", 0, "nixos-config", 0)
	if err != nil {
		t.Fatalf("proxyStats summary: %v", err)
	}
	if !strings.Contains(srv.capturedQuery, "view=summary") {
		t.Errorf("query %q does not contain view=summary", srv.capturedQuery)
	}
	if !strings.Contains(srv.capturedQuery, "repo=nixos-config") {
		t.Errorf("query %q does not contain repo=nixos-config", srv.capturedQuery)
	}
}

// TestProxyStats_Returns404AsError verifies that a 404 from the server is
// surfaced as an error with the error message.
func TestProxyStats_Returns404AsError(t *testing.T) {
	respBody := `{"error":"session \"ghost\" not found"}`
	srv := &fakeStatsServer{response: []byte(respBody), statusCode: http.StatusNotFound}
	mock := newMockUnixServer(t, srv.handler().ServeHTTP)

	_, err := proxyStats(mock.apiURL(), "detail", "ghost", 0, "", 0)
	if err == nil {
		t.Fatal("expected non-nil error for 404 response")
	}
	if !strings.Contains(err.Error(), "ghost") && !strings.Contains(err.Error(), "404") {
		t.Errorf("error %q does not mention ghost or 404", err.Error())
	}
}

// ── runStatsProxy integration tests ──────────────────────────────────────────

// TestRunStatsProxy_Doomloops verifies that when PRISM_HOST_API is set and
// --doomloops is passed, runStats sends GET /stats?view=doomloops to the server
// and renders the result.
func TestRunStatsProxy_Doomloops(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	events := []db.Event{
		{
			ID:          "evt-1",
			SessionName: "nixos-config@main",
			Type:        "doom_loop_detected",
			Payload:     `{"tool":"bash","pattern":"git status","count":5,"timestampMs":0}`,
			CreatedAt:   now.Add(-1 * time.Hour),
		},
	}
	eventsJSON, _ := json.Marshal(events)
	respBody, _ := json.Marshal(map[string]any{"events": events})

	srv, apiURL := startFakeStatsServer(t, respBody)

	t.Setenv("PRISM_HOST_API", apiURL)
	_ = eventsJSON // suppress unused warning
	_ = srv

	statsCmd.Flags().Set("doomloops", "true")        //nolint:errcheck
	defer statsCmd.Flags().Set("doomloops", "false") //nolint:errcheck

	out := captureStdout(t, func() {
		if err := runStats(statsCmd, nil); err != nil {
			t.Errorf("runStats: %v", err)
		}
	})

	if !strings.Contains(out, "Doom Loop Events") {
		t.Errorf("output missing 'Doom Loop Events' heading\ngot:\n%s", out)
	}
	if !strings.Contains(srv.capturedQuery, "view=doomloops") {
		t.Errorf("proxy did not send view=doomloops; query = %q", srv.capturedQuery)
	}
}

// TestRunStatsProxy_DoomloopsJSON verifies that --doomloops --json emits the
// raw response JSON, byte-identical to what the server returned.
func TestRunStatsProxy_DoomloopsJSON(t *testing.T) {
	events := []db.Event{
		{
			ID:          "evt-json",
			SessionName: "nixos-config@main",
			Type:        "doom_loop_detected",
			Payload:     `{"tool":"bash","pattern":"git diff","count":3,"timestampMs":0}`,
			CreatedAt:   time.Now().Add(-30 * time.Minute),
		},
	}
	respBody, _ := json.Marshal(map[string]any{"events": events})

	_, apiURL := startFakeStatsServer(t, respBody)

	t.Setenv("PRISM_HOST_API", apiURL)

	statsCmd.Flags().Set("doomloops", "true") //nolint:errcheck
	statsCmd.Flags().Set("json", "true")      //nolint:errcheck
	defer func() {
		statsCmd.Flags().Set("doomloops", "false") //nolint:errcheck
		statsCmd.Flags().Set("json", "false")      //nolint:errcheck
	}()

	out := captureStdout(t, func() {
		if err := runStats(statsCmd, nil); err != nil {
			t.Errorf("runStats --doomloops --json: %v", err)
		}
	})

	// Output should be valid JSON containing "events" key.
	var parsed map[string]json.RawMessage
	trimmed := strings.TrimSpace(out)
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\noutput: %s", err, out)
	}
	if _, ok := parsed["events"]; !ok {
		t.Errorf("--json output missing 'events' key; got: %s", out)
	}
}

// TestRunStatsProxy_Summary verifies that prism stats (no flags) via proxy
// renders a session table when PRISM_HOST_API is set.
func TestRunStatsProxy_Summary(t *testing.T) {
	sessions := []db.Session{
		{
			InstanceID:  "aaaa1111-2222-3333-4444-555555555555",
			SessionName: "nixos-config@main",
			Repo:        "nixos-config",
			Worktree:    "/tmp/w",
			Harness:     "pi",
			StartedAt:   time.Now().Add(-1 * time.Hour),
		},
	}
	respBody, _ := json.Marshal(map[string]any{"sessions": sessions})

	srv, apiURL := startFakeStatsServer(t, respBody)

	t.Setenv("PRISM_HOST_API", apiURL)

	out := captureStdout(t, func() {
		if err := runStats(statsCmd, nil); err != nil {
			t.Errorf("runStats (summary proxy): %v", err)
		}
	})

	if !strings.Contains(srv.capturedQuery, "view=summary") {
		t.Errorf("proxy did not send view=summary; query = %q", srv.capturedQuery)
	}
	// The rendered output should contain the session name.
	if !strings.Contains(out, "nixos-config@main") {
		t.Errorf("output missing session name\ngot:\n%s", out)
	}
}

// TestRunStatsProxy_SummaryJSON verifies that prism stats --json (no event flags)
// emits the proxy response JSON.
func TestRunStatsProxy_SummaryJSON(t *testing.T) {
	sessions := []db.Session{
		{
			InstanceID:  "bbbb1111-2222-3333-4444-555555555555",
			SessionName: "nixos-config@feat",
			Repo:        "nixos-config",
			Worktree:    "/tmp/w",
			Harness:     "pi",
			StartedAt:   time.Now().Add(-2 * time.Hour),
		},
	}
	respBody, _ := json.Marshal(map[string]any{"sessions": sessions})

	_, apiURL := startFakeStatsServer(t, respBody)

	t.Setenv("PRISM_HOST_API", apiURL)

	statsCmd.Flags().Set("json", "true")        //nolint:errcheck
	defer statsCmd.Flags().Set("json", "false") //nolint:errcheck

	out := captureStdout(t, func() {
		if err := runStats(statsCmd, nil); err != nil {
			t.Errorf("runStats --json (summary proxy): %v", err)
		}
	})

	var parsed map[string]json.RawMessage
	trimmed := strings.TrimSpace(out)
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\noutput: %s", err, out)
	}
	if _, ok := parsed["sessions"]; !ok {
		t.Errorf("--json output missing 'sessions' key; got: %s", out)
	}
}

// TestRunStatsProxy_Detail_RendersOutcome verifies that a `prism stats
// <session>` detail lookup via the proxy path renders the token/cost fields
// from the outcome the host-API view=detail response now carries, instead of
// the old fixed "token data requires host DB access" stub line (issue #2582).
func TestRunStatsProxy_Detail_RendersOutcome(t *testing.T) {
	sess := db.Session{
		InstanceID:  "cccc1111-2222-3333-4444-555555555555",
		SessionName: "nixos-config@detail",
		Repo:        "nixos-config",
		Worktree:    "/tmp/w",
		Harness:     "pi",
		StartedAt:   time.Now().Add(-1 * time.Hour),
	}
	outcome := &db.SpawnOutcome{
		InstanceID:            sess.InstanceID,
		TokensInputTotal:      12345,
		TokensOutputTotal:     6789,
		TokensCacheReadTotal:  1000,
		TokensCacheWriteTotal: 500,
		CostUSDTotal:          0,
	}
	respBody, _ := json.Marshal(map[string]any{"session": sess, "outcome": outcome})

	_, apiURL := startFakeStatsServer(t, respBody)
	t.Setenv("PRISM_HOST_API", apiURL)

	out := captureStdout(t, func() {
		if err := runStats(statsCmd, []string{"nixos-config@detail"}); err != nil {
			t.Errorf("runStats (detail proxy): %v", err)
		}
	})

	if strings.Contains(out, "requires host DB access") {
		t.Errorf("output still contains the old stub line:\n%s", out)
	}
	if !strings.Contains(out, "12K") {
		t.Errorf("output missing rendered input token count\ngot:\n%s", out)
	}
	if !strings.Contains(out, "$0.00") {
		t.Errorf("output should render an explicit $0.00 cost for a zero-cost subscription run, not omit it\ngot:\n%s", out)
	}
}

// TestRunStatsProxy_Detail_OutcomeNotYetAvailable verifies that a detail
// lookup for a session with no spawn_outcome row yet (still active) renders
// an explicit "not yet available" message rather than presenting zero values
// as real data (issue #2582 edge case).
func TestRunStatsProxy_Detail_OutcomeNotYetAvailable(t *testing.T) {
	sess := db.Session{
		InstanceID:  "dddd1111-2222-3333-4444-555555555555",
		SessionName: "nixos-config@active",
		Repo:        "nixos-config",
		Worktree:    "/tmp/w",
		Harness:     "pi",
		StartedAt:   time.Now().Add(-1 * time.Minute),
	}
	respBody, _ := json.Marshal(map[string]any{"session": sess, "outcome": nil})

	_, apiURL := startFakeStatsServer(t, respBody)
	t.Setenv("PRISM_HOST_API", apiURL)

	out := captureStdout(t, func() {
		if err := runStats(statsCmd, []string{"nixos-config@active"}); err != nil {
			t.Errorf("runStats (detail proxy, no outcome): %v", err)
		}
	})

	if !strings.Contains(out, "not yet available") {
		t.Errorf("output missing 'not yet available' message for a session with no spawn_outcome row\ngot:\n%s", out)
	}
	if strings.Contains(out, "requires host DB access") {
		t.Errorf("output still contains the old stub line:\n%s", out)
	}
}

// TestRunStatsProxy_GroupByFallsThrough verifies that --group-by skips the
// proxy and uses the local DB path even when PRISM_HOST_API is set.
// This guards against the regression where --group-by was silently dropped.
func TestRunStatsProxy_GroupByFallsThrough(t *testing.T) {
	_ = openStatsTestDB(t) // sets up empty DB, unsets PRISM_HOST_API via Setenv

	// Stand up a server that records requests — it should NOT be contacted
	// when --group-by is set.
	requestCount := 0
	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"sessions":[]}`)) //nolint:errcheck
	})

	// Set PRISM_HOST_API after openStatsTestDB cleared it.
	t.Setenv("PRISM_HOST_API", srv.apiURL())

	statsCmd.Flags().Set("group-by", "harness") //nolint:errcheck
	defer statsCmd.Flags().Set("group-by", "")  //nolint:errcheck

	_ = captureStdout(t, func() {
		// Uses local DB (empty) — should produce output from runStatsGroupBy,
		// not from the proxy. Any error is acceptable; no request to proxy.
		_ = runStats(statsCmd, nil)
	})

	if requestCount > 0 {
		t.Errorf("proxy server received %d request(s) for --group-by; should use local DB", requestCount)
	}
}

// TestRunStatsProxy_DaysHistoricalFallsThrough verifies that --days N with no
// event flags and no session arg skips the proxy (uses runStatsHistorical on
// the local DB) even when PRISM_HOST_API is set.
func TestRunStatsProxy_DaysHistoricalFallsThrough(t *testing.T) {
	_ = openStatsTestDB(t)

	requestCount := 0
	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"sessions":[]}`)) //nolint:errcheck
	})

	t.Setenv("PRISM_HOST_API", srv.apiURL())

	statsCmd.Flags().Set("days", "7")       //nolint:errcheck
	defer statsCmd.Flags().Set("days", "0") //nolint:errcheck

	_ = captureStdout(t, func() {
		_ = runStats(statsCmd, nil)
	})

	if requestCount > 0 {
		t.Errorf("proxy server received %d request(s) for --days historical; should use local DB", requestCount)
	}
}

// TestRunStatsProxy_HostPathUnchanged verifies that when PRISM_HOST_API is
// NOT set, runStats does not contact any server (the proxy path is not taken).
func TestRunStatsProxy_HostPathUnchanged(t *testing.T) {
	_ = openStatsTestDB(t)
	// PRISM_HOST_API is cleared by openStatsTestDB via t.Setenv.

	statsCmd.Flags().Set("doomloops", "true")        //nolint:errcheck
	defer statsCmd.Flags().Set("doomloops", "false") //nolint:errcheck

	// Stand up a server that records requests — it should NOT be contacted.
	requestCount := 0
	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"events":[]}`))
	})

	// Explicitly NOT setting PRISM_HOST_API — the test DB is used instead.
	_ = srv

	out := captureStdout(t, func() {
		_ = runStats(statsCmd, nil)
	})

	if requestCount > 0 {
		t.Errorf("proxy server received %d request(s) when PRISM_HOST_API is unset", requestCount)
	}
	if !strings.Contains(out, "Doom Loop Events") {
		t.Errorf("output missing 'Doom Loop Events' heading for direct-DB path; got:\n%s", out)
	}
}

// ── compare / abtest / --abtest proxy paths (#2098) ───────────────────────────
//
// These tests exercise the host-API proxy dispatch added in #2098 for
// `prism stats compare`, `prism stats abtest <group>`, and
// `prism stats --abtest`. The strongest guarantee is byte-identical output
// between the direct-DB path and the proxy path: each test renders the same
// seeded DB both ways and asserts the bytes match. The fake /stats server
// delegates to the same db helpers the real sidecar handler calls
// (db.ResolveSessionArg, db.AssembleCompareRun, db.AbtestGroupSessions,
// db.AbtestPairsAll), so the comparison spans a real HTTP + JSON round-trip.

// newCompareFlagsCmd returns a cobra.Command carrying the same flags the real
// compare/abtest subcommands register, isolated from the global command's
// flag state so each test can set flags without cross-test pollution.
func newCompareFlagsCmd() *cobra.Command {
	c := &cobra.Command{Use: "compare"}
	c.Flags().StringSlice("axes", nil, "")
	c.Flags().String("format", "table", "")
	c.Flags().Bool("diff-only", false, "")
	c.Flags().String("sort", "", "")
	c.Flags().Bool("include-inputs", false, "")
	c.Flags().Bool("include-rubric", false, "")
	return c
}

// startStatsProxyDBServer stands up a Unix-socket /stats server that serves the
// compare / abtest / abtest_list views by delegating to the same db helpers the
// real sidecar handler uses. It is a faithful in-process stand-in for the host
// sidecar, letting the proxy tests assert byte-identical output against the
// direct-DB path across a real HTTP + JSON round-trip.
func startStatsProxyDBServer(t *testing.T, d *db.DB) string {
	t.Helper()
	writeErr := func(w http.ResponseWriter, status int, msg string) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
	}
	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		switch q.Get("view") {
		case "compare":
			ids := q["id"]
			if len(ids) < 2 {
				writeErr(w, http.StatusBadRequest, "view=compare requires at least 2 id params")
				return
			}
			runs := make([]db.CompareRunData, 0, len(ids))
			for _, id := range ids {
				sess, err := d.ResolveSessionArg(id, false)
				if err != nil {
					writeErr(w, http.StatusNotFound, err.Error())
					return
				}
				runs = append(runs, d.AssembleCompareRun(sess))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"runs": runs})
		case "abtest":
			sessions, err := d.AbtestGroupSessions(q.Get("group"))
			if err != nil {
				writeErr(w, http.StatusNotFound, err.Error())
				return
			}
			runs := make([]db.CompareRunData, 0, len(sessions))
			for _, sess := range sessions {
				runs = append(runs, d.AssembleCompareRun(sess))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"runs": runs})
		case "abtest_list":
			pairs, err := d.AbtestPairsAll()
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			if pairs == nil {
				pairs = []db.AbtestPairRow{}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"pairs": pairs})
		default:
			writeErr(w, http.StatusBadRequest, "unknown view")
		}
	})
	return srv.apiURL()
}

// seedTwoCompareRuns seeds two terminal sessions (with token/cost events and a
// spawn_inputs row) and returns their instance IDs. Both sessions are
// `finished` so the compare engine computes an outcome on the fly.
func seedTwoCompareRuns(t *testing.T, d *db.DB) (idA, idB string) {
	t.Helper()
	startedAt := time.Now().Add(-5 * time.Minute).Truncate(time.Second)

	inputsA := &db.SpawnInputs{ProfileName: strPtrTest("opus-4-7"), HarnessFlag: strPtrTest("pi")}
	inputsB := &db.SpawnInputs{ProfileName: strPtrTest("opus-4-8"), HarnessFlag: strPtrTest("pi")}

	idA = seedCompareSession(t, d, "nixos-config@run-a", startedAt, agent.StateFinished, inputsA)
	idB = seedCompareSession(t, d, "nixos-config@run-b", startedAt.Add(time.Second), agent.StateFinished, inputsB)

	writeAssistantTurn(t, d, "nixos-config@run-a", idA, startedAt.Add(10*time.Second), 1500, 700, 300, 150, 0.12)
	writeToolCall(t, d, "nixos-config@run-a", idA, "bash", startedAt.Add(20*time.Second))
	writeAssistantTurn(t, d, "nixos-config@run-b", idB, startedAt.Add(11*time.Second), 2200, 900, 410, 205, 0.31)
	writeToolCall(t, d, "nixos-config@run-b", idB, "edit", startedAt.Add(21*time.Second))
	return idA, idB
}

func strPtrTest(s string) *string { return &s }

// renderCompareDirect runs `prism stats compare` against the direct-DB path and
// returns the captured stdout. PRISM_HOST_API must be unset on entry.
func renderCompareDirect(t *testing.T, cmd *cobra.Command, args ...string) string {
	t.Helper()
	t.Setenv("PRISM_HOST_API", "")
	return captureStdout(t, func() {
		if err := runStatsCompare(cmd, args); err != nil {
			t.Fatalf("runStatsCompare (direct): %v", err)
		}
	})
}

// renderCompareProxy runs `prism stats compare` against the proxy path (server
// at apiURL) and returns the captured stdout.
func renderCompareProxy(t *testing.T, cmd *cobra.Command, apiURL string, args ...string) string {
	t.Helper()
	t.Setenv("PRISM_HOST_API", apiURL)
	return captureStdout(t, func() {
		if err := runStatsCompare(cmd, args); err != nil {
			t.Fatalf("runStatsCompare (proxy): %v", err)
		}
	})
}

// TestStatsCompareProxy_ByteIdentical_Table is the core #2098 AC: the table
// output of `prism stats compare A B` must be byte-identical between the
// direct-DB path and the host-API proxy path.
func TestStatsCompareProxy_ByteIdentical_Table(t *testing.T) {
	d := openStatsTestDB(t)
	idA, idB := seedTwoCompareRuns(t, d)
	apiURL := startStatsProxyDBServer(t, d)

	direct := renderCompareDirect(t, newCompareFlagsCmd(), idA, idB)
	proxy := renderCompareProxy(t, newCompareFlagsCmd(), apiURL, idA, idB)

	if direct != proxy {
		t.Errorf("compare table output not byte-identical between direct and proxy paths\n--- DIRECT ---\n%s\n--- PROXY ---\n%s", direct, proxy)
	}
	if !strings.Contains(direct, "run-a") || !strings.Contains(direct, "run-b") {
		t.Errorf("expected both session names in output; got:\n%s", direct)
	}
}

// TestStatsCompareProxy_ByteIdentical_FormatsAndFlags asserts byte-identical
// output across --format json/csv and the --diff-only / --axes /
// --include-inputs / --include-rubric flags (ACs #5 and #6).
func TestStatsCompareProxy_ByteIdentical_FormatsAndFlags(t *testing.T) {
	d := openStatsTestDB(t)
	idA, idB := seedTwoCompareRuns(t, d)
	apiURL := startStatsProxyDBServer(t, d)

	type variant struct {
		name  string
		setup func(c *cobra.Command)
	}
	variants := []variant{
		{"json", func(c *cobra.Command) { _ = c.Flags().Set("format", "json") }},
		{"csv", func(c *cobra.Command) { _ = c.Flags().Set("format", "csv") }},
		{"diff-only", func(c *cobra.Command) { _ = c.Flags().Set("diff-only", "true") }},
		{"axes", func(c *cobra.Command) { _ = c.Flags().Set("axes", "tokens_input,cost_usd,tool_call") }},
		{"include-inputs-off", func(c *cobra.Command) { _ = c.Flags().Set("include-inputs", "false") }},
		{"include-rubric", func(c *cobra.Command) { _ = c.Flags().Set("include-rubric", "true") }},
	}

	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			dc := newCompareFlagsCmd()
			v.setup(dc)
			direct := renderCompareDirect(t, dc, idA, idB)

			pc := newCompareFlagsCmd()
			v.setup(pc)
			proxy := renderCompareProxy(t, pc, apiURL, idA, idB)

			if direct != proxy {
				t.Errorf("%s: output not byte-identical\n--- DIRECT ---\n%s\n--- PROXY ---\n%s", v.name, direct, proxy)
			}
		})
	}
}

// TestStatsCompareProxy_ByteIdentical_ThreeWay covers the 3+ run path
// (MIN/MAX annotations, no Δ column) through the proxy.
func TestStatsCompareProxy_ByteIdentical_ThreeWay(t *testing.T) {
	d := openStatsTestDB(t)
	idA, idB := seedTwoCompareRuns(t, d)
	startedAt := time.Now().Add(-4 * time.Minute).Truncate(time.Second)
	idC := seedCompareSession(t, d, "nixos-config@run-c", startedAt, agent.StateFinished, &db.SpawnInputs{ProfileName: strPtrTest("haiku")})
	writeAssistantTurn(t, d, "nixos-config@run-c", idC, startedAt.Add(9*time.Second), 800, 300, 100, 50, 0.05)
	apiURL := startStatsProxyDBServer(t, d)

	direct := renderCompareDirect(t, newCompareFlagsCmd(), idA, idB, idC)
	proxy := renderCompareProxy(t, newCompareFlagsCmd(), apiURL, idA, idB, idC)
	if direct != proxy {
		t.Errorf("3-way compare not byte-identical\n--- DIRECT ---\n%s\n--- PROXY ---\n%s", direct, proxy)
	}
}

// TestStatsCompareProxy_404 verifies that an unresolvable instance via the
// proxy surfaces as an error (AC: 404 case).
func TestStatsCompareProxy_404(t *testing.T) {
	d := openStatsTestDB(t)
	idA, _ := seedTwoCompareRuns(t, d)
	apiURL := startStatsProxyDBServer(t, d)

	t.Setenv("PRISM_HOST_API", apiURL)
	err := runStatsCompare(newCompareFlagsCmd(), []string{idA, "ghost-instance-that-does-not-exist"})
	if err == nil {
		t.Fatal("expected error for unresolvable instance via proxy, got nil")
	}
	if !strings.Contains(err.Error(), "ghost-instance-that-does-not-exist") {
		t.Errorf("error should name the unresolvable arg; got: %v", err)
	}
}

// TestStatsAbtestProxy_ByteIdentical seeds a session group and asserts
// `prism stats abtest <group>` is byte-identical between direct and proxy.
func TestStatsAbtestProxy_ByteIdentical(t *testing.T) {
	d := openStatsTestDB(t)
	seedTwoCompareRuns(t, d)
	groupID, err := d.RegisterGroup("nixos-config@main")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}
	if err := d.SetGroupID("nixos-config@run-a", groupID); err != nil {
		t.Fatalf("SetGroupID run-a: %v", err)
	}
	if err := d.SetGroupID("nixos-config@run-b", groupID); err != nil {
		t.Fatalf("SetGroupID run-b: %v", err)
	}
	apiURL := startStatsProxyDBServer(t, d)

	t.Setenv("PRISM_HOST_API", "")
	direct := captureStdout(t, func() {
		if err := runStatsAbtest(newCompareFlagsCmd(), []string{groupID}); err != nil {
			t.Fatalf("runStatsAbtest (direct): %v", err)
		}
	})
	t.Setenv("PRISM_HOST_API", apiURL)
	proxy := captureStdout(t, func() {
		if err := runStatsAbtest(newCompareFlagsCmd(), []string{groupID}); err != nil {
			t.Fatalf("runStatsAbtest (proxy): %v", err)
		}
	})
	if direct != proxy {
		t.Errorf("abtest output not byte-identical\n--- DIRECT ---\n%s\n--- PROXY ---\n%s", direct, proxy)
	}
}

// seedAbtestPair seeds two finished sessions sharing an abtest_pair_id in their
// spawn_inputs rows, so they surface in `prism stats --abtest`.
func seedAbtestPair(t *testing.T, d *db.DB, pairID string) {
	t.Helper()
	startedAt := time.Now().Add(-6 * time.Minute).Truncate(time.Second)
	inA := &db.SpawnInputs{ProfileName: strPtrTest("opus-4-7"), AbtestPairID: strPtrTest(pairID)}
	inB := &db.SpawnInputs{ProfileName: strPtrTest("opus-4-8"), AbtestPairID: strPtrTest(pairID)}
	idA := seedCompareSession(t, d, "nixos-config@pair-a", startedAt, agent.StateFinished, inA)
	idB := seedCompareSession(t, d, "nixos-config@pair-b", startedAt.Add(time.Second), agent.StateFinished, inB)
	writeAssistantTurn(t, d, "nixos-config@pair-a", idA, startedAt.Add(10*time.Second), 1500, 700, 300, 150, 0.12)
	writeAssistantTurn(t, d, "nixos-config@pair-b", idB, startedAt.Add(11*time.Second), 2200, 900, 410, 205, 0.31)
}

// TestStatsAbtestFlagProxy_ByteIdentical seeds an abtest pair and asserts
// `prism stats --abtest` is byte-identical between direct and proxy.
func TestStatsAbtestFlagProxy_ByteIdentical(t *testing.T) {
	d := openStatsTestDB(t)
	seedAbtestPair(t, d, "pair-2098-aaaa-bbbb-cccc-dddddddddddd")
	apiURL := startStatsProxyDBServer(t, d)

	t.Setenv("PRISM_HOST_API", "")
	direct := captureStdout(t, func() {
		if err := runStatsAbtestFlag(false); err != nil {
			t.Fatalf("runStatsAbtestFlag (direct): %v", err)
		}
	})
	t.Setenv("PRISM_HOST_API", apiURL)
	proxy := captureStdout(t, func() {
		if err := runStatsAbtestFlag(false); err != nil {
			t.Fatalf("runStatsAbtestFlag (proxy): %v", err)
		}
	})
	if direct != proxy {
		t.Errorf("--abtest listing not byte-identical\n--- DIRECT ---\n%s\n--- PROXY ---\n%s", direct, proxy)
	}
	if !strings.Contains(direct, "A/B Test Pairs") {
		t.Errorf("expected pairs table header; got:\n%s", direct)
	}
}

// TestStatsAbtestFlagProxy_Empty verifies the empty-pairs message is rendered
// identically via the proxy.
func TestStatsAbtestFlagProxy_Empty(t *testing.T) {
	d := openStatsTestDB(t)
	apiURL := startStatsProxyDBServer(t, d)

	t.Setenv("PRISM_HOST_API", apiURL)
	out := captureStdout(t, func() {
		if err := runStatsAbtestFlag(false); err != nil {
			t.Fatalf("runStatsAbtestFlag (proxy, empty): %v", err)
		}
	})
	if !strings.Contains(out, "no abtest pairs recorded") {
		t.Errorf("expected 'no abtest pairs recorded' via proxy; got:\n%s", out)
	}
}

// ── over-broad-fix guards: PRISM_HOST_API unset → direct DB, no proxy ──────────

// TestStatsCompareProxy_UnsetUsesDirectDB verifies that with PRISM_HOST_API
// unset, `prism stats compare` does not contact any server (direct-DB path).
func TestStatsCompareProxy_UnsetUsesDirectDB(t *testing.T) {
	d := openStatsTestDB(t) // clears PRISM_HOST_API
	idA, idB := seedTwoCompareRuns(t, d)

	requestCount := 0
	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"runs":[]}`))
	})
	_ = srv // deliberately NOT setting PRISM_HOST_API

	out := captureStdout(t, func() {
		if err := runStatsCompare(newCompareFlagsCmd(), []string{idA, idB}); err != nil {
			t.Fatalf("runStatsCompare (direct): %v", err)
		}
	})
	if requestCount != 0 {
		t.Errorf("proxy server received %d request(s); compare must use direct DB when PRISM_HOST_API unset", requestCount)
	}
	if !strings.Contains(out, "run-a") {
		t.Errorf("expected direct-DB rendered output; got:\n%s", out)
	}
}

// TestStatsAbtestProxy_UnsetUsesDirectDB verifies the same for
// `prism stats abtest <group>`.
func TestStatsAbtestProxy_UnsetUsesDirectDB(t *testing.T) {
	d := openStatsTestDB(t)
	seedTwoCompareRuns(t, d)
	groupID, err := d.RegisterGroup("nixos-config@main")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}
	if err := d.SetGroupID("nixos-config@run-a", groupID); err != nil {
		t.Fatalf("SetGroupID run-a: %v", err)
	}
	if err := d.SetGroupID("nixos-config@run-b", groupID); err != nil {
		t.Fatalf("SetGroupID run-b: %v", err)
	}

	requestCount := 0
	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"runs":[]}`))
	})
	_ = srv

	out := captureStdout(t, func() {
		if err := runStatsAbtest(newCompareFlagsCmd(), []string{groupID}); err != nil {
			t.Fatalf("runStatsAbtest (direct): %v", err)
		}
	})
	if requestCount != 0 {
		t.Errorf("proxy server received %d request(s); abtest must use direct DB when PRISM_HOST_API unset", requestCount)
	}
	if !strings.Contains(out, "run-a") {
		t.Errorf("expected direct-DB rendered output; got:\n%s", out)
	}
}

// TestStatsAbtestFlagProxy_UnsetUsesDirectDB verifies the same for
// `prism stats --abtest`.
func TestStatsAbtestFlagProxy_UnsetUsesDirectDB(t *testing.T) {
	d := openStatsTestDB(t)
	seedAbtestPair(t, d, "pair-noproxy-aaaa-bbbb-cccc-dddddddddddd")

	requestCount := 0
	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"pairs":[]}`))
	})
	_ = srv

	out := captureStdout(t, func() {
		if err := runStatsAbtestFlag(false); err != nil {
			t.Fatalf("runStatsAbtestFlag (direct): %v", err)
		}
	})
	if requestCount != 0 {
		t.Errorf("proxy server received %d request(s); --abtest must use direct DB when PRISM_HOST_API unset", requestCount)
	}
	if !strings.Contains(out, "A/B Test Pairs") {
		t.Errorf("expected direct-DB pairs table; got:\n%s", out)
	}
}
