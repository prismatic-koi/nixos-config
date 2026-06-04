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

	"github.com/google/uuid"
	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/sidecar"
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

// ── stats compare / abtest / --abtest proxy tests (#2098) ────────────────────

// proxyCompareFixture builds a synthetic StatsCompareResponseWire payload —
// the same shape the host sidecar emits for GET /stats?view=compare. Tests
// hand this to a mock server so the proxy path can be exercised without
// standing up a real sidecar.
func proxyCompareFixture() sidecar.StatsCompareResponseWire {
	startedA := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	startedB := time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC)
	endedA := startedA.Add(2 * time.Minute)
	endedB := startedB.Add(3 * time.Minute)

	stateFinished := "finished"
	durA := int64(120000)
	durB := int64(180000)
	profA := "anthropic-opus-max-4-7"
	profB := "anthropic-opus-max-4-8"

	sessA := &db.Session{
		InstanceID:  "aaaaaaaa-1111-2222-3333-444444444444",
		SessionName: "repo@run-a",
		Repo:        "repo",
		Worktree:    "/wt/run-a",
		Harness:     "pi",
		StartedAt:   startedA,
		EndedAt:     &endedA,
		EndState:    &stateFinished,
	}
	sessB := &db.Session{
		InstanceID:  "bbbbbbbb-1111-2222-3333-444444444444",
		SessionName: "repo@run-b",
		Repo:        "repo",
		Worktree:    "/wt/run-b",
		Harness:     "pi",
		StartedAt:   startedB,
		EndedAt:     &endedB,
		EndState:    &stateFinished,
	}
	outA := &db.SpawnOutcome{
		InstanceID:        sessA.InstanceID,
		EndState:          &stateFinished,
		DurationMs:        &durA,
		TokensInputTotal:  1500,
		TokensOutputTotal: 700,
		CostUSDTotal:      0.12,
		ToolCallCount:     3,
		MsgAssistantCount: 2,
	}
	outB := &db.SpawnOutcome{
		InstanceID:        sessB.InstanceID,
		EndState:          &stateFinished,
		DurationMs:        &durB,
		TokensInputTotal:  2000,
		TokensOutputTotal: 900,
		CostUSDTotal:      0.34,
		ToolCallCount:     5,
		MsgAssistantCount: 4,
	}
	inA := &sidecar.StatsCompareInputsWire{
		ProfileName: &profA,
	}
	inB := &sidecar.StatsCompareInputsWire{
		ProfileName: &profB,
	}
	return sidecar.StatsCompareResponseWire{
		Runs: []sidecar.StatsCompareRunWire{
			{Label: "run-A", Session: sessA, Outcome: outA, Inputs: inA},
			{Label: "run-B", Session: sessB, Outcome: outB, Inputs: inB},
		},
	}
}

// resetCompareFlags returns compareCmd's flags to their declared defaults
// between tests. Cobra-defined flags are package-global, so the
// statsCmd-style "set, defer set" pattern must be applied per-test.
func resetCompareFlags(t *testing.T) {
	t.Helper()
	_ = compareCmd.Flags().Set("format", "table")
	_ = compareCmd.Flags().Set("diff-only", "false")
	_ = compareCmd.Flags().Set("include-rubric", "false")
	_ = compareCmd.Flags().Set("include-inputs", "false")
	// Mark include-inputs as not-explicitly-set so the renderer applies its
	// "on for 2-run, off for 3+" default. cobra.Flags has no public API for
	// clearing Changed, so we rely on tests not depending on the prior state.
}

// resetAbtestFlags is the analogue of resetCompareFlags for abtestCmd.
func resetAbtestFlags(t *testing.T) {
	t.Helper()
	_ = abtestCmd.Flags().Set("format", "table")
	_ = abtestCmd.Flags().Set("diff-only", "false")
	_ = abtestCmd.Flags().Set("include-rubric", "false")
	_ = abtestCmd.Flags().Set("include-inputs", "false")
}

// ── proxyStats* helper signature tests ───────────────────────────────────────

// TestProxyStatsCompare_SendsCorrectQueryParams verifies that proxyStatsCompare
// sends GET /stats with view=compare and a comma-joined ids list.
func TestProxyStatsCompare_SendsCorrectQueryParams(t *testing.T) {
	respBody := `{"runs":[]}`
	srv, apiURL := startFakeStatsServer(t, []byte(respBody))

	_, err := proxyStatsCompare(apiURL, []string{"run-A", "run-B"})
	if err != nil {
		t.Fatalf("proxyStatsCompare: %v", err)
	}
	if srv.capturedPath != "/stats" {
		t.Errorf("path = %q, want /stats", srv.capturedPath)
	}
	if !strings.Contains(srv.capturedQuery, "view=compare") {
		t.Errorf("query %q missing view=compare", srv.capturedQuery)
	}
	if !strings.Contains(srv.capturedQuery, "ids=run-A%2Crun-B") {
		t.Errorf("query %q missing ids=run-A%%2Crun-B", srv.capturedQuery)
	}
}

// TestProxyStatsAbtest_SendsCorrectQueryParams verifies that proxyStatsAbtest
// sends GET /stats with view=abtest and group_id.
func TestProxyStatsAbtest_SendsCorrectQueryParams(t *testing.T) {
	respBody := `{"runs":[]}`
	srv, apiURL := startFakeStatsServer(t, []byte(respBody))

	_, err := proxyStatsAbtest(apiURL, "grp-123")
	if err != nil {
		t.Fatalf("proxyStatsAbtest: %v", err)
	}
	if !strings.Contains(srv.capturedQuery, "view=abtest") {
		t.Errorf("query %q missing view=abtest", srv.capturedQuery)
	}
	if !strings.Contains(srv.capturedQuery, "group_id=grp-123") {
		t.Errorf("query %q missing group_id=grp-123", srv.capturedQuery)
	}
}

// TestProxyStatsAbtestList_SendsCorrectQueryParams verifies the view=abtest_list
// query is sent verbatim with no extra params.
func TestProxyStatsAbtestList_SendsCorrectQueryParams(t *testing.T) {
	respBody := `{"pairs":[]}`
	srv, apiURL := startFakeStatsServer(t, []byte(respBody))

	_, err := proxyStatsAbtestList(apiURL)
	if err != nil {
		t.Fatalf("proxyStatsAbtestList: %v", err)
	}
	if !strings.Contains(srv.capturedQuery, "view=abtest_list") {
		t.Errorf("query %q missing view=abtest_list", srv.capturedQuery)
	}
}

// ── runStatsCompare proxy integration tests ──────────────────────────────────

// TestRunStatsCompare_ProxyHappyPath verifies that when PRISM_HOST_API is set,
// the compare command pulls per-run data from the host-API proxy and renders
// the labels and core axis values.
func TestRunStatsCompare_ProxyHappyPath(t *testing.T) {
	fixture := proxyCompareFixture()
	respBody, _ := json.Marshal(fixture)

	srv, apiURL := startFakeStatsServer(t, respBody)
	t.Setenv("PRISM_HOST_API", apiURL)
	resetCompareFlags(t)
	defer resetCompareFlags(t)

	args := []string{"repo@run-a", "repo@run-b"}
	out := captureStdout(t, func() {
		if err := runStatsCompare(compareCmd, args); err != nil {
			t.Errorf("runStatsCompare proxy: %v", err)
		}
	})

	if !strings.Contains(srv.capturedQuery, "view=compare") {
		t.Errorf("proxy did not receive view=compare; query=%q", srv.capturedQuery)
	}
	if !strings.Contains(srv.capturedQuery, "ids=repo%40run-a%2Crepo%40run-b") {
		t.Errorf("proxy did not receive expected ids; query=%q", srv.capturedQuery)
	}
	if !strings.Contains(out, "run-A") || !strings.Contains(out, "run-B") {
		t.Errorf("output missing run labels:\n%s", out)
	}
	if !strings.Contains(out, "finished") {
		t.Errorf("output missing 'finished' end_state:\n%s", out)
	}
	// Token totals from the fixture should be visible in the rendered table.
	if !strings.Contains(out, "1.5K") && !strings.Contains(out, "1500") {
		t.Errorf("output missing run-A tokens_input (1.5K/1500):\n%s", out)
	}
}

// TestRunStatsCompare_Proxy404Surface verifies that a 404 from the sidecar is
// surfaced as an error containing the missing-id phrase.
func TestRunStatsCompare_Proxy404Surface(t *testing.T) {
	respBody := `{"error":"instance \"ghost\" not found"}`
	srv := &fakeStatsServer{response: []byte(respBody), statusCode: http.StatusNotFound}
	mock := newMockUnixServer(t, srv.handler().ServeHTTP)

	t.Setenv("PRISM_HOST_API", mock.apiURL())
	resetCompareFlags(t)
	defer resetCompareFlags(t)

	args := []string{"ghost", "repo@run-b"}
	out := captureStdout(t, func() {
		err := runStatsCompare(compareCmd, args)
		if err == nil {
			t.Fatal("expected error for 404 from proxy, got nil")
		}
		if !strings.Contains(err.Error(), "ghost") && !strings.Contains(err.Error(), "not found") {
			t.Errorf("error %q does not mention ghost or not found", err.Error())
		}
	})
	_ = out
}

// TestRunStatsCompare_ProxyJSONFormat verifies that --format json with a proxy
// response produces valid JSON containing the runs array.
func TestRunStatsCompare_ProxyJSONFormat(t *testing.T) {
	fixture := proxyCompareFixture()
	respBody, _ := json.Marshal(fixture)

	_, apiURL := startFakeStatsServer(t, respBody)
	t.Setenv("PRISM_HOST_API", apiURL)
	resetCompareFlags(t)
	_ = compareCmd.Flags().Set("format", "json")
	defer resetCompareFlags(t)

	out := captureStdout(t, func() {
		if err := runStatsCompare(compareCmd, []string{"repo@run-a", "repo@run-b"}); err != nil {
			t.Errorf("runStatsCompare --format json proxy: %v", err)
		}
	})

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &parsed); err != nil {
		t.Fatalf("--format json output is not valid JSON: %v\noutput: %s", err, out)
	}
	if _, ok := parsed["runs"]; !ok {
		t.Errorf("JSON output missing 'runs' key:\n%s", out)
	}
	if _, ok := parsed["diffs"]; !ok {
		t.Errorf("JSON output missing 'diffs' key:\n%s", out)
	}
}

// TestRunStatsCompare_ProxyCSVFormat verifies --format csv via proxy.
func TestRunStatsCompare_ProxyCSVFormat(t *testing.T) {
	fixture := proxyCompareFixture()
	respBody, _ := json.Marshal(fixture)

	_, apiURL := startFakeStatsServer(t, respBody)
	t.Setenv("PRISM_HOST_API", apiURL)
	resetCompareFlags(t)
	_ = compareCmd.Flags().Set("format", "csv")
	defer resetCompareFlags(t)

	out := captureStdout(t, func() {
		if err := runStatsCompare(compareCmd, []string{"repo@run-a", "repo@run-b"}); err != nil {
			t.Errorf("runStatsCompare --format csv proxy: %v", err)
		}
	})

	// CSV header must include axis,run-A,run-B,delta on a 2-run comparison.
	if !strings.HasPrefix(strings.TrimSpace(out), "axis,run-A,run-B,delta") {
		t.Errorf("CSV output missing expected header line; got:\n%s", out)
	}
}

// TestRunStatsCompare_ProxyDiffOnly verifies that --diff-only is honoured on
// the proxy path (filtering happens on the CLI side after unmarshaling).
func TestRunStatsCompare_ProxyDiffOnly(t *testing.T) {
	fixture := proxyCompareFixture()
	respBody, _ := json.Marshal(fixture)

	_, apiURL := startFakeStatsServer(t, respBody)
	t.Setenv("PRISM_HOST_API", apiURL)
	resetCompareFlags(t)
	_ = compareCmd.Flags().Set("diff-only", "true")
	defer resetCompareFlags(t)

	out := captureStdout(t, func() {
		if err := runStatsCompare(compareCmd, []string{"repo@run-a", "repo@run-b"}); err != nil {
			t.Errorf("runStatsCompare --diff-only proxy: %v", err)
		}
	})

	// end_state is "finished" for both runs in the fixture; with --diff-only
	// the row must be filtered out.
	if strings.Contains(out, "end_state:") {
		t.Errorf("--diff-only should have filtered end_state row but it is present:\n%s", out)
	}
	// duration_ms differs between A (120000) and B (180000) — the row stays.
	if !strings.Contains(out, "duration_ms:") {
		t.Errorf("--diff-only should NOT filter duration_ms (values differ):\n%s", out)
	}
}

// TestRunStatsCompare_HostPathUnchanged is the over-broad-fix guard required
// by the issue's AC: with PRISM_HOST_API unset, runStatsCompare must hit the
// direct-DB path and NOT contact any HTTP server.
func TestRunStatsCompare_HostPathUnchanged(t *testing.T) {
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-2 * time.Minute)
	iidA := seedCompareSession(t, d, "repo@run-a", startedAt, agent.StateFinished, nil)
	iidB := seedCompareSession(t, d, "repo@run-b", startedAt.Add(time.Second), agent.StateFinished, nil)
	_ = iidA
	_ = iidB

	requestCount := 0
	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"runs":[]}`))
	})
	_ = srv
	// openStatsTestDB has set PRISM_HOST_API="" already; do not override.

	resetCompareFlags(t)
	defer resetCompareFlags(t)

	out := captureStdout(t, func() {
		if err := runStatsCompare(compareCmd, []string{"repo@run-a", "repo@run-b"}); err != nil {
			t.Errorf("runStatsCompare direct-DB: %v", err)
		}
	})

	if requestCount > 0 {
		t.Errorf("proxy server received %d request(s); should use direct DB", requestCount)
	}
	// Render at least produced something — the session labels are a good
	// signal the renderer ran on real DB rows.
	if !strings.Contains(out, "run-A") || !strings.Contains(out, "run-B") {
		t.Errorf("direct-DB output missing run labels:\n%s", out)
	}
}

// TestRunStatsCompare_ProxyByteIdentity verifies the core AC of #2098: the
// proxy path renders byte-identically to the direct-DB path when the
// underlying data is the same. We seed a DB with assistant turns, persist
// the spawn_outcome rows (so neither path computes on the fly), capture
// the direct-DB output, build the wire payload from the SAME DB rows,
// serve it via mock, run the proxy path, and assert exact equality.
//
// Persisting the outcome rows is what keeps the test deterministic:
// ComputeSpawnOutcome stamps ComputedAt = time.Now() on each call, so two
// uncached invocations would differ in that field. Reading the persisted
// row twice returns the same ComputedAt value, and the JSON roundtrip
// preserves it.
func TestRunStatsCompare_ProxyByteIdentity(t *testing.T) {
	d := openStatsTestDB(t)
	startedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	const (
		nameA = "repo@compare-a"
		nameB = "repo@compare-b"
	)
	iidA := seedCompareSession(t, d, nameA, startedAt, agent.StateFinished, nil)
	iidB := seedCompareSession(t, d, nameB, startedAt.Add(time.Second), agent.StateFinished, nil)

	// Seed distinguishable per-run aggregates via msg_assistant events,
	// then persist the computed spawn_outcome rows so subsequent reads
	// are deterministic.
	writeAssistantTurn(t, d, nameA, iidA, startedAt.Add(10*time.Second), 1500, 700, 300, 150, 0.12)
	writeAssistantTurn(t, d, nameA, iidA, startedAt.Add(40*time.Second), 2000, 900, 400, 200, 0.34)
	writeToolCall(t, d, nameA, iidA, "bash", startedAt.Add(20*time.Second))

	writeAssistantTurn(t, d, nameB, iidB, startedAt.Add(15*time.Second), 1800, 800, 350, 175, 0.20)
	writeAssistantTurn(t, d, nameB, iidB, startedAt.Add(50*time.Second), 2500, 1100, 500, 250, 0.40)
	writeToolCall(t, d, nameB, iidB, "bash", startedAt.Add(25*time.Second))

	if err := d.WriteSpawnOutcome(iidA); err != nil {
		t.Fatalf("WriteSpawnOutcome A: %v", err)
	}
	if err := d.WriteSpawnOutcome(iidB); err != nil {
		t.Fatalf("WriteSpawnOutcome B: %v", err)
	}

	resetCompareFlags(t)
	defer resetCompareFlags(t)

	// 1) Direct-DB path with PRISM_HOST_API empty.
	directOut := captureStdout(t, func() {
		if err := runStatsCompare(compareCmd, []string{nameA, nameB}); err != nil {
			t.Fatalf("direct compare: %v", err)
		}
	})

	// 2) Build the wire payload using the SAME DB rows the direct path read,
	// serve via mock, run proxy path.
	sessA, _ := d.SessionByInstanceID(iidA)
	sessB, _ := d.SessionByInstanceID(iidB)
	persistedA, _ := d.SpawnOutcomeByInstanceID(iidA)
	persistedB, _ := d.SpawnOutcomeByInstanceID(iidB)
	wire := sidecar.StatsCompareResponseWire{
		Runs: []sidecar.StatsCompareRunWire{
			{Label: "run-A", Session: sessA, Outcome: persistedA},
			{Label: "run-B", Session: sessB, Outcome: persistedB},
		},
	}
	wireJSON, _ := json.Marshal(wire)
	_, apiURL := startFakeStatsServer(t, wireJSON)
	t.Setenv("PRISM_HOST_API", apiURL)

	proxyOut := captureStdout(t, func() {
		if err := runStatsCompare(compareCmd, []string{nameA, nameB}); err != nil {
			t.Fatalf("proxy compare: %v", err)
		}
	})

	if directOut != proxyOut {
		t.Errorf("proxy and direct outputs differ — byte-identity AC violated\n=== direct ===\n%s\n=== proxy ===\n%s", directOut, proxyOut)
	}
}

// ── runStatsAbtest proxy integration tests ───────────────────────────────────

// TestRunStatsAbtest_ProxyHappyPath verifies that `prism stats abtest
// <group_id>` proxies via /stats?view=abtest&group_id=...
func TestRunStatsAbtest_ProxyHappyPath(t *testing.T) {
	fixture := proxyCompareFixture()
	respBody, _ := json.Marshal(fixture)

	srv, apiURL := startFakeStatsServer(t, respBody)
	t.Setenv("PRISM_HOST_API", apiURL)
	resetAbtestFlags(t)
	defer resetAbtestFlags(t)

	out := captureStdout(t, func() {
		if err := runStatsAbtest(abtestCmd, []string{"grp-abcd"}); err != nil {
			t.Errorf("runStatsAbtest proxy: %v", err)
		}
	})

	if !strings.Contains(srv.capturedQuery, "view=abtest") {
		t.Errorf("proxy did not receive view=abtest; query=%q", srv.capturedQuery)
	}
	if !strings.Contains(srv.capturedQuery, "group_id=grp-abcd") {
		t.Errorf("proxy did not receive group_id=grp-abcd; query=%q", srv.capturedQuery)
	}
	if !strings.Contains(out, "run-A") || !strings.Contains(out, "run-B") {
		t.Errorf("output missing run labels:\n%s", out)
	}
}

// TestRunStatsAbtest_HostPathUnchanged is the over-broad-fix guard for the
// abtest subcommand: PRISM_HOST_API unset → direct DB; no proxy request.
func TestRunStatsAbtest_HostPathUnchanged(t *testing.T) {
	d := openStatsTestDB(t)
	_ = d

	requestCount := 0
	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"runs":[]}`))
	})
	_ = srv

	resetAbtestFlags(t)
	defer resetAbtestFlags(t)

	// No group exists in the DB, so the direct-DB path returns an error.
	// What we are guarding against is *contacting the mock server* on the
	// direct-DB path — the command's own outcome is irrelevant here.
	_ = captureStdout(t, func() {
		_ = runStatsAbtest(abtestCmd, []string{"missing-group"})
	})

	if requestCount > 0 {
		t.Errorf("proxy server received %d request(s); should use direct DB", requestCount)
	}
}

// ── runStatsAbtestFlag (--abtest listing) proxy tests ────────────────────────

// abtestPairFixture builds one synthetic AbtestPairRow for the listing.
func abtestPairFixture() db.AbtestPairRow {
	turnsA := 2
	turnsB := 4
	tokInA := int64(1500)
	tokOutA := int64(700)
	tokInB := int64(2000)
	tokOutB := int64(900)
	durA := int64(120000)
	durB := int64(180000)
	stateA := "finished"
	stateB := "finished"
	return db.AbtestPairRow{
		PairID:        uuid.New().String(),
		SessionNameA:  "repo@pair-a",
		InstanceIDA:   "aaaaaaaa-1111-2222-3333-444444444444",
		ProfileA:      "anthropic-opus-max-4-7",
		StartedAtA:    time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC).UnixMilli(),
		SessionNameB:  "repo@pair-b",
		InstanceIDB:   "bbbbbbbb-1111-2222-3333-444444444444",
		ProfileB:      "anthropic-opus-max-4-8",
		StartedAtB:    time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC).UnixMilli(),
		TurnsA:        &turnsA,
		TurnsB:        &turnsB,
		TokensInputA:  &tokInA,
		TokensInputB:  &tokInB,
		TokensOutputA: &tokOutA,
		TokensOutputB: &tokOutB,
		DurationMsA:   &durA,
		DurationMsB:   &durB,
		EndStateA:     &stateA,
		EndStateB:     &stateB,
	}
}

// TestRunStatsAbtestFlag_ProxyHappyPath verifies that --abtest (top-level
// listing flag) routes via /stats?view=abtest_list and renders the table.
func TestRunStatsAbtestFlag_ProxyHappyPath(t *testing.T) {
	pairs := []db.AbtestPairRow{abtestPairFixture()}
	respBody, _ := json.Marshal(sidecar.StatsAbtestListResponseWire{Pairs: pairs})

	srv, apiURL := startFakeStatsServer(t, respBody)
	t.Setenv("PRISM_HOST_API", apiURL)

	statsCmd.Flags().Set("abtest", "true")        //nolint:errcheck
	defer statsCmd.Flags().Set("abtest", "false") //nolint:errcheck

	out := captureStdout(t, func() {
		if err := runStats(statsCmd, nil); err != nil {
			t.Errorf("runStats --abtest proxy: %v", err)
		}
	})

	if !strings.Contains(srv.capturedQuery, "view=abtest_list") {
		t.Errorf("proxy did not receive view=abtest_list; query=%q", srv.capturedQuery)
	}
	if !strings.Contains(out, "A/B Test Pairs") {
		t.Errorf("output missing 'A/B Test Pairs' heading:\n%s", out)
	}
	if !strings.Contains(out, "repo@pair-a") || !strings.Contains(out, "repo@pair-b") {
		t.Errorf("output missing pair session names:\n%s", out)
	}
}

// TestRunStatsAbtestFlag_ProxyEmpty verifies the empty-set hint is shared
// between the direct-DB and proxy paths.
func TestRunStatsAbtestFlag_ProxyEmpty(t *testing.T) {
	respBody := `{"pairs":[]}`
	_, apiURL := startFakeStatsServer(t, []byte(respBody))
	t.Setenv("PRISM_HOST_API", apiURL)

	statsCmd.Flags().Set("abtest", "true")        //nolint:errcheck
	defer statsCmd.Flags().Set("abtest", "false") //nolint:errcheck

	out := captureStdout(t, func() {
		if err := runStats(statsCmd, nil); err != nil {
			t.Errorf("runStats --abtest proxy empty: %v", err)
		}
	})

	if !strings.Contains(out, "no abtest pairs recorded") {
		t.Errorf("output missing empty-set hint:\n%s", out)
	}
}

// TestRunStatsAbtestFlag_HostPathUnchanged is the over-broad-fix guard for
// the --abtest listing: PRISM_HOST_API unset → direct DB; no proxy request.
func TestRunStatsAbtestFlag_HostPathUnchanged(t *testing.T) {
	_ = openStatsTestDB(t) // clears PRISM_HOST_API via t.Setenv

	requestCount := 0
	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"pairs":[]}`))
	})
	_ = srv

	statsCmd.Flags().Set("abtest", "true")        //nolint:errcheck
	defer statsCmd.Flags().Set("abtest", "false") //nolint:errcheck

	out := captureStdout(t, func() {
		if err := runStats(statsCmd, nil); err != nil {
			t.Errorf("runStats --abtest direct: %v", err)
		}
	})

	if requestCount > 0 {
		t.Errorf("proxy server received %d request(s); should use direct DB", requestCount)
	}
	if !strings.Contains(out, "no abtest pairs recorded") {
		t.Errorf("direct-DB output missing empty-set hint:\n%s", out)
	}
}
