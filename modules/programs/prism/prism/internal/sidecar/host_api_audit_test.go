package sidecar

// host_api_audit_test.go — issue #2618.
//
// GET /audit is the host-side half of the `prism audit` proxy branch. Two
// properties keep it inside the boundary that already exists, and this file
// is the regression guard for both:
//
//   - Role gate. The route is coordinator-only, matching /db/query. A worker
//     gets 403. (The whole coordinator-only surface is also pinned in
//     host_api_role_gate_test.go; this file adds the paired "and a
//     coordinator is served" half.)
//   - Server-side type filter. Audit rows are agent_events rows with
//     type = 'audit'. The predicate lives inside db.QueryAuditEvents and is
//     not reachable from any request parameter, so the route cannot be
//     widened into a general cross-session conversation-payload reader.
//
// # Isolation contract (#1608)
//
// Every sidecar here is built through the helpers in
// host_api_role_gate_test.go, which use sidecartest.NewIsolated(t, "") — so
// $XDG_STATE_HOME points at a t.TempDir(), the DB is test-scoped, and every
// session name carries the "prism-test" prefix.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

const (
	auditTestRepo    = "prism-test-audit"
	auditTestSession = "prism-test-audit@main"
	auditTestWorker  = "prism-test-audit@feature-x"
)

// auditTestBase is the fixed clock origin for seeded rows. Using a fixed
// instant keeps the `since` assertions deterministic.
var auditTestBase = time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)

// newAuditSidecar returns a coordinator sidecar plus the DB behind it, so a
// test can seed rows and then call the endpoint that reads them.
func newAuditSidecar(t *testing.T) (*Sidecar, *db.DB) {
	t.Helper()
	bus := sidecartest.NewIsolated(t, "")
	if err := bus.DB.UpsertStatusSeedRootAgentName(
		auditTestSession, auditTestRepo, "/tmp/"+auditTestRepo, "active", nil, nil,
		"coordinator", "", "",
	); err != nil {
		t.Fatalf("seed coordinator status: %v", err)
	}
	sc := newRoleGateSidecarWithDB(t, auditTestSession, auditTestRepo,
		"coordinator", failingStub, bus.DB)
	return sc, bus.DB
}

// writeAuditTestEvent writes one agent_events row of an arbitrary type.
func writeAuditTestEvent(t *testing.T, d *db.DB, session, typ, payloadJSON string, at time.Time) {
	t.Helper()
	if err := d.WriteEvent(db.Event{
		ID:          fmt.Sprintf("%s-%s-%d", session, typ, at.UnixMilli()),
		SessionName: session,
		Repo:        auditTestRepo,
		Worktree:    "/tmp/" + auditTestRepo,
		Type:        typ,
		Payload:     payloadJSON,
		CreatedAt:   at,
	}); err != nil {
		t.Fatalf("WriteEvent(%s/%s): %v", session, typ, err)
	}
}

// seedAuditMix seeds a deliberately adversarial mix: audit rows alongside
// non-audit rows of several types, each non-audit row carrying a `command`
// field in its payload so a dropped type filter would let a broad --pattern
// match it. If the type filter is ever widened, these rows are what leaks.
func seedAuditMix(t *testing.T, d *db.DB) {
	t.Helper()

	// Audit rows — the only rows the endpoint may ever return.
	writeAuditTestEvent(t, d, auditTestSession, "audit",
		`{"tool":"bash","command":"gh pr merge 2618 --squash","sessionName":"`+auditTestSession+`"}`,
		auditTestBase)
	writeAuditTestEvent(t, d, auditTestSession, "audit",
		`{"tool":"bash","command":"git push --force","sessionName":"`+auditTestSession+`"}`,
		auditTestBase.Add(1*time.Minute))
	writeAuditTestEvent(t, d, auditTestWorker, "audit",
		`{"tool":"bash","command":"nix build .#prism","sessionName":"`+auditTestWorker+`"}`,
		auditTestBase.Add(2*time.Minute))
	writeAuditTestEvent(t, d, auditTestSession, "audit",
		`{"command":"prism checkin other-repo@main","sessionName":"`+auditTestSession+`","target":"other-repo@main","grant":"checkin-privileged-repo"}`,
		auditTestBase.Add(3*time.Minute))

	// Non-audit rows — conversation payloads and tool traffic. None of these
	// may ever appear in a response.
	writeAuditTestEvent(t, d, auditTestSession, "tool_call",
		`{"command":"cat ~/.ssh/id_ed25519","tool":"bash"}`,
		auditTestBase.Add(4*time.Minute))
	writeAuditTestEvent(t, d, auditTestWorker, "assistant_turn",
		`{"command":"secret worker reasoning","text":"private"}`,
		auditTestBase.Add(5*time.Minute))
	writeAuditTestEvent(t, d, auditTestWorker, "permission_denied",
		`{"command":"rm -rf /","tool":"bash"}`,
		auditTestBase.Add(6*time.Minute))
	writeAuditTestEvent(t, d, auditTestSession, "user_message",
		`{"command":"gh pr merge 2618 --squash","text":"looks like an audit row but is not"}`,
		auditTestBase.Add(7*time.Minute))
}

// getAuditEvents calls GET /audit with the given raw query string and decodes
// the events array. It fails the test on any non-200.
func getAuditEvents(t *testing.T, sc *Sidecar, rawQuery string) []db.Event {
	t.Helper()
	path := "/audit"
	if rawQuery != "" {
		path += "?" + rawQuery
	}
	rr := doHostAPI(t, sc, http.MethodGet, path, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, body = %q, want 200", path, rr.Code, rr.Body.String())
	}
	var resp struct {
		Events []db.Event `json:"events"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("GET %s: unmarshal %q: %v", path, rr.Body.String(), err)
	}
	return resp.Events
}

// ── the type filter ──────────────────────────────────────────────────────────

// TestHostAPIAudit_NeverReturnsNonAuditRows is the headline security
// assertion of #2618. The endpoint reads agent_events, which holds every
// session's conversation payloads. Only `type = 'audit'` rows may leave it.
//
// Each case passes a hostile value for one or more of the four parameters the
// endpoint accepts, plus a set of parameters the endpoint does not accept at
// all (a caller trying to smuggle in a type override or raw SQL). None may
// widen the result set beyond audit rows.
func TestHostAPIAudit_NeverReturnsNonAuditRows(t *testing.T) {
	sc, d := newAuditSidecar(t)
	seedAuditMix(t, d)

	cases := []struct {
		name  string
		query string
	}{
		{"no filters at all", ""},
		{"empty values for every parameter", "session=&since=&pattern=&limit="},
		{"wildcard pattern", "pattern=%25"},
		{"underscore wildcard pattern", "pattern=_"},
		{"pattern closing the LIKE and OR-ing the type predicate",
			"pattern=%25%27+OR+type%3D%27tool_call"},
		{"pattern commenting out the rest of the WHERE clause",
			"pattern=x%27+--+"},
		{"session name as a SQL fragment",
			"session=%27+OR+1%3D1+--+"},
		{"session name naming the type column",
			"session=%27+OR+type+LIKE+%27%25"},
		{"limit smuggling a UNION",
			"limit=1+UNION+SELECT+*+FROM+agent_events"},
		{"since smuggling a SQL fragment",
			"since=0+OR+1%3D1"},
		{"negative limit", "limit=-1"},
		{"zero limit", "limit=0"},
		{"enormous limit", "limit=999999999"},
		{"negative since", "since=-9999999999999"},
		{"type override parameter", "type=tool_call"},
		{"type override alongside a live filter", "type=assistant_turn&pattern=command"},
		{"sql parameter borrowed from /db/query",
			"sql=SELECT+*+FROM+agent_events"},
		{"conditions parameter", "conditions=1%3D1&pattern=%25"},
		{"repeated pattern parameters", "pattern=%25&pattern=nothing-matches-this"},
		{"repeated session parameters",
			"session=" + auditTestSession + "&session=" + auditTestWorker},
		{"every parameter hostile at once",
			"session=%27+OR+1%3D1+--+&since=-1&pattern=%25%27+OR+1%3D1+--+&limit=999999&type=tool_call"},
	}

	sawSomeAuditRow := false
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A malformed since/limit is a 400, not a widened result — both
			// outcomes are acceptable here; what is not acceptable is a 200
			// carrying a non-audit row.
			path := "/audit"
			if tc.query != "" {
				path += "?" + tc.query
			}
			rr := doHostAPI(t, sc, http.MethodGet, path, "")
			if rr.Code == http.StatusBadRequest {
				return
			}
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %q, want 200 or 400", rr.Code, rr.Body.String())
			}
			var resp struct {
				Events []db.Event `json:"events"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal %q: %v", rr.Body.String(), err)
			}
			for _, e := range resp.Events {
				if e.Type != "audit" {
					t.Errorf("query %q returned a %q row (id=%s, payload=%s); "+
						"the type='audit' filter must be server-side and unreachable from a parameter",
						tc.query, e.Type, e.ID, e.Payload)
				}
			}
			if len(resp.Events) > 0 {
				sawSomeAuditRow = true
			}
		})
	}

	// Guard against a vacuous pass: if every case returned zero rows the loop
	// above would assert nothing at all.
	if !sawSomeAuditRow {
		t.Fatal("no case returned any row — the assertions above were vacuous")
	}
}

// TestHostAPIAudit_TypeFilterHoldsWithNoAuditRows covers the complement: a DB
// that holds only non-audit rows must produce an empty array, not a fallback
// to "everything".
func TestHostAPIAudit_TypeFilterHoldsWithNoAuditRows(t *testing.T) {
	sc, d := newAuditSidecar(t)
	writeAuditTestEvent(t, d, auditTestSession, "tool_call",
		`{"command":"gh pr merge 1 --squash"}`, auditTestBase)
	writeAuditTestEvent(t, d, auditTestWorker, "assistant_turn",
		`{"command":"private reasoning"}`, auditTestBase.Add(time.Minute))

	for _, q := range []string{"", "pattern=%25", "limit=1000", "session=" + auditTestSession} {
		got := getAuditEvents(t, sc, q)
		if len(got) != 0 {
			t.Errorf("query %q returned %d row(s), want 0 — no audit rows exist", q, len(got))
		}
	}
}

// ── filter parity with the direct-DB path ────────────────────────────────────

// TestHostAPIAudit_MatchesQueryAuditEvents pins the proxy-path AC: for the
// same inputs, the endpoint returns the same rows the host-mode path returns.
// The host-mode path is db.QueryAuditEvents, so this compares the endpoint
// against it directly, filter matrix and all.
func TestHostAPIAudit_MatchesQueryAuditEvents(t *testing.T) {
	sc, d := newAuditSidecar(t)
	seedAuditMix(t, d)

	sinceMid := auditTestBase.Add(2 * time.Minute).UnixMilli()

	cases := []struct {
		name    string
		query   string
		session string
		since   int64
		pattern string
		limit   int
	}{
		{"no filters", "", "", 0, "", 0},
		{"session only", "session=" + auditTestSession, auditTestSession, 0, "", 0},
		{"session with no rows", "session=prism-test-audit@absent", "prism-test-audit@absent", 0, "", 0},
		{"since only", fmt.Sprintf("since=%d", sinceMid), "", sinceMid, "", 0},
		{"pattern only", "pattern=git+push", "", 0, "git push", 0},
		{"pattern is case-insensitive", "pattern=GIT+PUSH", "", 0, "GIT PUSH", 0},
		{"pattern matching nothing", "pattern=zzz-no-match", "", 0, "zzz-no-match", 0},
		{"limit only", "limit=2", "", 0, "", 2},
		{"session and limit", "session=" + auditTestSession + "&limit=1", auditTestSession, 0, "", 1},
		{"every filter at once",
			fmt.Sprintf("session=%s&since=%d&pattern=%s&limit=5", auditTestSession, auditTestBase.UnixMilli(), "merge"),
			auditTestSession, auditTestBase.UnixMilli(), "merge", 5},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want, err := d.QueryAuditEvents(tc.session, tc.since, tc.pattern, tc.limit)
			if err != nil {
				t.Fatalf("QueryAuditEvents: %v", err)
			}
			got := getAuditEvents(t, sc, tc.query)
			if len(got) != len(want) {
				t.Fatalf("row count = %d, want %d (direct path)\n got: %s\nwant: %s",
					len(got), len(want), auditIDs(got), auditIDs(want))
			}
			for i := range want {
				if got[i].ID != want[i].ID {
					t.Errorf("row %d: id = %q, want %q (order must match the direct path)",
						i, got[i].ID, want[i].ID)
				}
				if got[i].Payload != want[i].Payload {
					t.Errorf("row %d: payload = %q, want %q", i, got[i].Payload, want[i].Payload)
				}
				if !got[i].CreatedAt.Equal(want[i].CreatedAt) {
					t.Errorf("row %d: created_at = %v, want %v", i, got[i].CreatedAt, want[i].CreatedAt)
				}
			}
		})
	}
}

func auditIDs(events []db.Event) string {
	ids := make([]string, 0, len(events))
	for _, e := range events {
		ids = append(ids, e.ID)
	}
	return fmt.Sprintf("%v", ids)
}

// TestHostAPIAudit_EmptyResultIsArrayNotNull keeps the CLI's no-results
// message working on the proxy route: a nil slice must marshal as [].
func TestHostAPIAudit_EmptyResultIsArrayNotNull(t *testing.T) {
	sc, _ := newAuditSidecar(t)

	rr := doHostAPI(t, sc, http.MethodGet, "/audit", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}
	var resp struct {
		Events *[]db.Event `json:"events"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal %q: %v", rr.Body.String(), err)
	}
	if resp.Events == nil {
		t.Fatalf("events was null; want an empty array (body = %q)", rr.Body.String())
	}
	if len(*resp.Events) != 0 {
		t.Errorf("events had %d entries, want 0", len(*resp.Events))
	}
}

// ── the role gate ────────────────────────────────────────────────────────────

// TestHostAPIAudit_DeniesWorker is the paired half of the coordinator-only
// gate: a worker gets 403 and reads nothing, even though audit rows exist.
func TestHostAPIAudit_DeniesWorker(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	if err := bus.DB.UpsertStatusSeedRootAgentName(
		auditTestWorker, auditTestRepo, "/tmp/"+auditTestRepo, "active", nil, nil,
		"worker", "", "",
	); err != nil {
		t.Fatalf("seed worker status: %v", err)
	}
	seedAuditMix(t, bus.DB)
	sc := newRoleGateSidecarWithDB(t, auditTestWorker, auditTestRepo,
		"worker", failingStub, bus.DB)

	rr := doHostAPI(t, sc, http.MethodGet, "/audit", "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %q, want 403", rr.Code, rr.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v (body = %q)", err, rr.Body.String())
	}
	if resp["error"] == "" {
		t.Errorf("403 body carried no error message: %q", rr.Body.String())
	}
}

// TestHostAPIAudit_DeniesUndeterminableRole covers the fail-closed property:
// a caller whose role the DB cannot answer, and whose name does not end in
// @main, is treated as a worker.
func TestHostAPIAudit_DeniesUndeterminableRole(t *testing.T) {
	sc := newRoleGateSidecar(t,
		"prism-test-audit@no-row", auditTestRepo, "", "", failingStub)

	rr := doHostAPI(t, sc, http.MethodGet, "/audit", "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %q, want 403", rr.Code, rr.Body.String())
	}
}

// TestHostAPIAudit_RejectsNonGet pins the method guard, so the route cannot be
// reached with a body-carrying verb.
func TestHostAPIAudit_RejectsNonGet(t *testing.T) {
	sc, d := newAuditSidecar(t)
	seedAuditMix(t, d)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rr := doHostAPI(t, sc, method, "/audit", "")
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /audit: status = %d, want 405", method, rr.Code)
		}
	}
}

// ── malformed input ──────────────────────────────────────────────────────────

// TestHostAPIAudit_RejectsMalformedNumerics asserts that a bad `since` or
// `limit` is refused rather than silently dropped. Dropping the filter would
// widen the result set past what the caller asked for.
func TestHostAPIAudit_RejectsMalformedNumerics(t *testing.T) {
	sc, d := newAuditSidecar(t)
	seedAuditMix(t, d)

	for _, q := range []string{"since=not-a-number", "limit=not-a-number", "limit=1.5", "since=9e99"} {
		rr := doHostAPI(t, sc, http.MethodGet, "/audit?"+q, "")
		if rr.Code != http.StatusBadRequest {
			t.Errorf("GET /audit?%s: status = %d, body = %q, want 400",
				q, rr.Code, rr.Body.String())
		}
	}
}

// TestHostAPIAudit_NoDBHandleIsNotAPanic covers the nil-DB path. A session
// named <repo>@main is admitted by the coordinator name heuristic with no DB
// evidence, so the handler can be reached with s.cfg.DB == nil.
func TestHostAPIAudit_NoDBHandleIsNotAPanic(t *testing.T) {
	sc := newRoleGateSidecarWithDB(t, auditTestSession, auditTestRepo,
		"coordinator", failingStub, nil)

	rr := doHostAPI(t, sc, http.MethodGet, "/audit", "")
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %q, want 500", rr.Code, rr.Body.String())
	}
}
