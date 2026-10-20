package sidecar

// host_api_event_caller_test.go — the caller check and the arg allowlist of
// the /event proxy, and the caller-role gates that a DB role must not open
// (#3207).
//
// The prism binary of each sidecar is a stub that writes its argv to a file,
// one arg per line. A refused request must leave the file absent: the
// handler ran no host `prism event`, which is the only writer that /event
// has. The run of an accepted argv against the real host command is tested
// in cmd/event_proxy_caller_test.go.

import (
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/sessionname"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

const (
	evtWorker  = "prism-test-evt@feature"
	evtSibling = "prism-test-evt@sibling"
	evtCoord   = "prism-test-evt@main"
	evtRoot    = "prism-test-evtroot"
	evtNewName = "prism-test-evtplanted"
)

// evtOthers are the rows of the other sessions in the DB of every evtSidecar.
var evtOthers = []struct{ name, worktree, role string }{
	{evtCoord, "/tmp/prism-test-evt/main", "coordinator"},
	{evtSibling, "/tmp/prism-test-evt/sibling", "worker"},
	{evtRoot, "/tmp/prism-test-evtroot", ""},
}

// evtSidecar builds an isolated sidecar with the start config (session,
// role, worktree). The DB holds a row for the caller with dbRole as
// root_agent_name, and the rows in evtOthers. The stub prism binary writes
// its argv to argvFile.
func evtSidecar(t *testing.T, session, role, worktree, dbRole string) (sc *Sidecar, d *db.DB, argvFile string) {
	t.Helper()
	bus := sidecartest.NewIsolated(t, "")
	d = bus.DB
	seed := func(name, wt, rootRole string) {
		if err := d.UpsertStatusSeedRootAgentName(name, sessionname.Repo(name), wt, "active", nil, nil, rootRole, "", ""); err != nil {
			t.Fatalf("seed %q: %v", name, err)
		}
	}
	seed(session, worktree, dbRole)
	for _, o := range evtOthers {
		if o.name != session {
			seed(o.name, o.worktree, o.role)
		}
	}

	argvFile = filepath.Join(t.TempDir(), "argv")
	stub := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > '" + argvFile + "'\n"
	stubPath := filepath.Join(t.TempDir(), "prism-stub")
	if err := os.WriteFile(stubPath, []byte(stub), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	sc = New(Config{
		SessionName:     session,
		Repo:            sessionname.Repo(session),
		Worktree:        worktree,
		HarnessURL:      "http://localhost:14000",
		DB:              d,
		Clock:           newTestClock(),
		AgentRole:       role,
		PrismBinaryPath: stubPath,
		Harness:         newSSEHarness(),
		Logger:          log.New(os.Stderr, "", 0),
	})
	return sc, d, argvFile
}

// evtArgv returns the argv that the stub recorded, or nil when the handler
// ran no host command.
func evtArgv(t *testing.T, argvFile string) []string {
	t.Helper()
	data, err := os.ReadFile(argvFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read argv: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// evtRows returns the columns of every agent_status row that /event can
// change, keyed by session name, so a test can compare the DB before and
// after a request.
func evtRows(t *testing.T, d *db.DB) map[string]string {
	t.Helper()
	names := []string{evtWorker, evtSibling, evtCoord, evtRoot, evtNewName}
	rows := make(map[string]string, len(names))
	for _, name := range names {
		st, err := d.CurrentStatus(name)
		if err != nil {
			t.Fatalf("CurrentStatus(%q): %v", name, err)
		}
		if st == nil {
			rows[name] = "<no row>"
			continue
		}
		deref := func(p *string) string {
			if p == nil {
				return "<null>"
			}
			return *p
		}
		rows[name] = fmt.Sprintf("repo=%q worktree=%q state=%q root_agent_name=%q ended=%v instance_id=%q",
			st.Repo, st.Worktree, st.State, deref(st.RootAgentName), st.EndedAt != nil, deref(st.InstanceID))
	}
	return rows
}

// evtBody returns the /event request body.
func evtBody(t *testing.T, kind, session string, args map[string]string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"kind": kind, "session": session, "args": args})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return string(b)
}

// evtKind is one kind with the args that its proxy client sends, and the
// flags that the host run must get for them.
type evtKind struct {
	kind      string
	args      map[string]string
	wantFlags []string
}

// evtKinds returns every kind of eventArgAllowlist, with the args of the
// proxy client in cmd/event.go for a session whose worktree is worktree.
func evtKinds(worktree string) []evtKind {
	return []evtKind{
		{"state-change", map[string]string{"state": "active", "worktree": worktree}, []string{"--state=active", "--worktree=" + worktree}},
		{"pane-died", map[string]string{"window": "agent", "exit-code": "1"}, []string{"--window=agent", "--exit-code=1"}},
		{"tmux-session-start", map[string]string{"worktree": worktree}, []string{"--worktree=" + worktree}},
		{"tmux-session-end", map[string]string{}, nil},
		{"compaction", map[string]string{}, nil},
		{"error", map[string]string{"message": "it broke"}, []string{"--message=it broke"}},
		{"doom-loop-detected", map[string]string{"tool": "bash", "pattern": "gh issue view", "count": "5"}, []string{"--tool=bash", "--pattern=gh issue view", "--count=5"}},
	}
}

// TestEvtKinds_CoverTheAllowlist keeps evtKinds in step with
// eventArgAllowlist, so a new kind cannot skip the tests below.
func TestEvtKinds_CoverTheAllowlist(t *testing.T) {
	var got []string
	for _, k := range evtKinds("/w") {
		got = append(got, k.kind)
		var keys []string
		for key := range k.args {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		want := slices.Clone(eventArgAllowlist[k.kind])
		slices.Sort(want)
		if !slices.Equal(keys, want) {
			t.Errorf("evtKinds %q args = %v, eventArgAllowlist = %v", k.kind, keys, want)
		}
	}
	slices.Sort(got)
	var want []string
	for kind := range eventArgAllowlist {
		want = append(want, kind)
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("evtKinds kinds = %v, eventArgAllowlist kinds = %v", got, want)
	}
}

// TestHostAPI_Event_OtherSessionRefused pins the caller check. A request for
// any session other than the sidecar's own gets 403, for a worker and for a
// coordinator, for every kind. The handler runs no host command, and no row
// changes or appears.
func TestHostAPI_Event_OtherSessionRefused(t *testing.T) {
	callers := []struct{ session, role string }{
		{evtWorker, "worker"},
		{evtCoord, "coordinator"},
	}
	for _, caller := range callers {
		for _, target := range []string{evtWorker, evtSibling, evtCoord, evtRoot, evtNewName} {
			if target == caller.session {
				continue
			}
			for _, k := range evtKinds(t.TempDir()) {
				t.Run(caller.role+"/"+target+"/"+k.kind, func(t *testing.T) {
					sc, d, argvFile := evtSidecar(t, caller.session, caller.role, t.TempDir(), caller.role)
					before := evtRows(t, d)

					rr := doHostAPI(t, sc, http.MethodPost, "/event", evtBody(t, k.kind, target, k.args))
					if rr.Code != http.StatusForbidden {
						t.Fatalf("status = %d, body = %s, want 403", rr.Code, rr.Body.String())
					}
					if msg := errorField(t, rr.Body.String()); !strings.Contains(msg, "only for itself") {
						t.Errorf("error %q, want the caller-check refusal", msg)
					}
					if argv := evtArgv(t, argvFile); argv != nil {
						t.Errorf("the handler ran a host command: %q", argv)
					}
					if after := evtRows(t, d); !maps.Equal(before, after) {
						t.Errorf("the DB changed:\nbefore %v\nafter  %v", before, after)
					}
				})
			}
		}
	}
}

// TestHostAPI_Event_OwnSessionForwardsAllowlistedArgs pins the accepted
// path: each kind for the caller's own session runs the host command with
// --session and the allowlisted flags only.
func TestHostAPI_Event_OwnSessionForwardsAllowlistedArgs(t *testing.T) {
	worktree := t.TempDir()
	for _, k := range evtKinds(worktree) {
		t.Run(k.kind, func(t *testing.T) {
			sc, _, argvFile := evtSidecar(t, evtWorker, "worker", worktree, "worker")

			rr := doHostAPI(t, sc, http.MethodPost, "/event", evtBody(t, k.kind, evtWorker, k.args))
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s, want 200", rr.Code, rr.Body.String())
			}
			want := append([]string{"event", k.kind, "--session=" + evtWorker}, k.wantFlags...)
			if got := evtArgv(t, argvFile); !slices.Equal(got, want) {
				t.Errorf("argv = %q, want %q", got, want)
			}
		})
	}
}

// TestHostAPI_Event_ArgNotInAllowlistRefused pins the arg allowlist. A key
// that is not in the list for the kind gets 400, and the handler runs no
// host command. agent-role is refused for every kind, so a worker cannot
// write root_agent_name. session is refused, so a second --session cannot
// override the caller check.
func TestHostAPI_Event_ArgNotInAllowlistRefused(t *testing.T) {
	worktree := t.TempDir()
	extras := map[string]string{
		"agent-role": "coordinator",
		"session":    evtCoord,
		"repo":       "prism-test-other",
		"bogus":      "x",
	}
	for _, k := range evtKinds(worktree) {
		for key, value := range extras {
			t.Run(k.kind+"/"+key, func(t *testing.T) {
				sc, d, argvFile := evtSidecar(t, evtWorker, "worker", worktree, "worker")
				before := evtRows(t, d)

				args := map[string]string{key: value}
				for ak, av := range k.args {
					args[ak] = av
				}
				rr := doHostAPI(t, sc, http.MethodPost, "/event", evtBody(t, k.kind, evtWorker, args))
				if rr.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, body = %s, want 400", rr.Code, rr.Body.String())
				}
				if msg := errorField(t, rr.Body.String()); !strings.Contains(msg, fmt.Sprintf("arg %q is not allowed", key)) {
					t.Errorf("error %q, want the allowlist refusal for %q", msg, key)
				}
				if argv := evtArgv(t, argvFile); argv != nil {
					t.Errorf("the handler ran a host command: %q", argv)
				}
				if after := evtRows(t, d); !maps.Equal(before, after) {
					t.Errorf("the DB changed:\nbefore %v\nafter  %v", before, after)
				}
			})
		}
	}
}

// TestHostAPI_Event_WorktreeIsTheStartWorktree pins the worktree check. A
// worktree that is not the start worktree of the sidecar gets 403. An empty
// worktree, or the start worktree in another spelling, runs the host command
// with the start worktree.
func TestHostAPI_Event_WorktreeIsTheStartWorktree(t *testing.T) {
	start := t.TempDir()
	chosen := t.TempDir()
	for _, kind := range []string{"state-change", "tmux-session-start"} {
		refused := []struct{ name, worktree string }{
			{"a directory the worker chose", chosen},
			{"the worktree of the coordinator", "/tmp/prism-test-evt/main"},
			{"a child of the start worktree", filepath.Join(start, "sub")},
			{"a relative path", "."},
		}
		for _, tc := range refused {
			t.Run(kind+"/refused/"+tc.name, func(t *testing.T) {
				sc, d, argvFile := evtSidecar(t, evtWorker, "worker", start, "worker")
				before := evtRows(t, d)

				args := map[string]string{"worktree": tc.worktree}
				if kind == "state-change" {
					args["state"] = "active"
				}
				rr := doHostAPI(t, sc, http.MethodPost, "/event", evtBody(t, kind, evtWorker, args))
				if rr.Code != http.StatusForbidden {
					t.Fatalf("status = %d, body = %s, want 403", rr.Code, rr.Body.String())
				}
				if msg := errorField(t, rr.Body.String()); !strings.Contains(msg, "only its own worktree") {
					t.Errorf("error %q, want the worktree refusal", msg)
				}
				if argv := evtArgv(t, argvFile); argv != nil {
					t.Errorf("the handler ran a host command: %q", argv)
				}
				if after := evtRows(t, d); !maps.Equal(before, after) {
					t.Errorf("the DB changed:\nbefore %v\nafter  %v", before, after)
				}
			})
		}

		for _, sent := range []string{"", start + "/", start + "/sub/.."} {
			t.Run(fmt.Sprintf("%s/accepted/%q", kind, sent), func(t *testing.T) {
				sc, _, argvFile := evtSidecar(t, evtWorker, "worker", start, "worker")
				rr := doHostAPI(t, sc, http.MethodPost, "/event",
					evtBody(t, kind, evtWorker, map[string]string{"worktree": sent}))
				if rr.Code != http.StatusOK {
					t.Fatalf("status = %d, body = %s, want 200", rr.Code, rr.Body.String())
				}
				if argv := evtArgv(t, argvFile); !slices.Contains(argv, "--worktree="+start) {
					t.Errorf("argv = %q, want --worktree=%s", argv, start)
				}
			})
		}
	}

	t.Run("no start worktree", func(t *testing.T) {
		sc, _, argvFile := evtSidecar(t, evtWorker, "worker", "", "worker")
		rr := doHostAPI(t, sc, http.MethodPost, "/event",
			evtBody(t, "tmux-session-start", evtWorker, map[string]string{"worktree": chosen}))
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, body = %s, want 500", rr.Code, rr.Body.String())
		}
		if argv := evtArgv(t, argvFile); argv != nil {
			t.Errorf("the handler ran a host command: %q", argv)
		}
	})
}

// evtCoordinatorGates is the requireCoordinator surface (the list in
// TestHostAPI_CoordinatorOnly_DeniesWorker) and /escalate for another
// session. Each target is in the repo of the caller, so only the caller-role
// gate can refuse a request with 403.
var evtCoordinatorGates = []struct{ method, path, body string }{
	{http.MethodPost, "/spawn", `{"branch":"feature-x","prompt":"do the thing"}`},
	{http.MethodPost, "/cleanup", `{"session":"prism-test-evt@sibling","yes":true}`},
	{http.MethodPost, "/close", `{"session":"prism-test-evt@sibling"}`},
	{http.MethodPost, "/merge", `{"pr":1}`},
	{http.MethodGet, "/merges", ""},
	{http.MethodPost, "/merges/cancel", `{"pr":1}`},
	{http.MethodPost, "/investigate", `{"prompt":"look into this"}`},
	{http.MethodGet, "/logs?session=prism-test-evt@sibling", ""},
	{http.MethodGet, "/db/query?sql=SELECT+1", ""},
	{http.MethodGet, "/db/schema", ""},
	{http.MethodGet, "/db/tables", ""},
	{http.MethodGet, "/audit", ""},
	{http.MethodPost, "/escalate", `{"prompt":"help","from":"prism-test-evt@sibling"}`},
}

// TestHostAPI_ForgedRole_CoordinatorGatesRefuseWorker is the escalation of
// #3207. A worker asks /event to seed root_agent_name=coordinator for itself,
// and /event refuses. The row then gets root_agent_name=coordinator by a
// direct write, which stands for a row forged before the fix. Each
// coordinator gate still refuses the worker, because the gates read the
// start config first.
//
// The DB allows one live coordinator row per repo
// (idx_active_coordinator_per_repo), so the test ends the coordinator row
// before the forge. The forge then fits a repo whose coordinator is not
// running.
func TestHostAPI_ForgedRole_CoordinatorGatesRefuseWorker(t *testing.T) {
	worktree := t.TempDir()
	for _, gate := range evtCoordinatorGates {
		t.Run(gate.method+" "+gate.path, func(t *testing.T) {
			sc, d, argvFile := evtSidecar(t, evtWorker, "worker", worktree, "worker")

			rr := doHostAPI(t, sc, http.MethodPost, "/event", evtBody(t, "tmux-session-start", evtWorker,
				map[string]string{"worktree": worktree, "agent-role": "coordinator"}))
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("/event with agent-role: status = %d, body = %s, want 400", rr.Code, rr.Body.String())
			}
			if role, _, _ := d.RootAgentName(evtWorker); role != "worker" {
				t.Fatalf("root_agent_name = %q after /event, want worker", role)
			}

			if err := d.SetEnded(evtCoord); err != nil {
				t.Fatalf("end the coordinator row: %v", err)
			}
			if err := d.UpsertStatusSeedRootAgentName(evtWorker, "prism-test-evt", worktree, "active", nil, nil, "coordinator", "", ""); err != nil {
				t.Fatalf("forge the DB role: %v", err)
			}
			rr = doHostAPI(t, sc, gate.method, gate.path, gate.body)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("%s %s: status = %d, body = %q, want 403 for a worker with a forged DB role",
					gate.method, gate.path, rr.Code, truncateForLog(rr.Body.String(), 200))
			}
			if argv := evtArgv(t, argvFile); argv != nil {
				t.Errorf("the handler ran a host command: %q", argv)
			}
		})
	}
}

// TestHostAPI_CoordinatorGates_AdmitCoordinator is the positive control of
// the test above. A coordinator passes every gate, also when its DB row
// holds a stale role, because the start config decides. Any status other
// than 403 shows that the gate admitted the caller. The root session of a
// non-worktree project is in
// TestHostAPI_NonWorktreeRoot_CoordinatorOnlyAdmitted.
func TestHostAPI_CoordinatorGates_AdmitCoordinator(t *testing.T) {
	for _, dbRole := range []string{"coordinator", "worker"} {
		for _, gate := range evtCoordinatorGates {
			t.Run("db role "+dbRole+"/"+gate.method+" "+gate.path, func(t *testing.T) {
				sc, _, _ := evtSidecar(t, evtCoord, "coordinator", t.TempDir(), dbRole)
				rr := doHostAPI(t, sc, gate.method, gate.path, gate.body)
				if rr.Code == http.StatusForbidden {
					t.Fatalf("%s %s: status = 403, body = %q, want the coordinator admitted",
						gate.method, gate.path, truncateForLog(rr.Body.String(), 200))
				}
			})
		}
	}
}
