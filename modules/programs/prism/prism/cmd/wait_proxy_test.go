package cmd

// Tests for the sandbox-aware wait-probe routing (#1500 review-code feedback).
//
// When PRISM_HOST_API is set, the wait probes for merge / review / spawn
// must talk to the sidecar's read-only wait-probe endpoints rather than
// opening the local (shadow) prism.db. These tests stand up a fake
// host-API server that mirrors /merges/by-pr, /sessions/status, and
// /groups/poll, then exercise observeAlreadyTerminal,
// waitForSpawnTerminal, and waitForReviewTerminal end-to-end through the
// proxy with PRISM_HOST_API pointed at the fake.

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// fakeWaitProbeServer mirrors the three host-API wait-probe endpoints.
// Tests stage responses by setting the *Resp fields and call the wait
// helper under test; the server records each request so tests can confirm
// the probe routed via HTTP rather than opening a shadow DB.
type fakeWaitProbeServer struct {
	mu       sync.Mutex
	requests []string

	// /merges/by-pr response. When mergeStatus=="" the server returns 404.
	mergePR     int
	mergeStatus string
	// /sessions/status response. When sessionState=="" the server returns 404.
	sessionName  string
	sessionState string
	// /groups/poll response.
	groupID         string
	groupCompleted  bool
	groupMembers    []db.Status
	groupResults    map[string]db.GroupMemberResult
}

func (s *fakeWaitProbeServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/merges/by-pr", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, "/merges/by-pr?"+r.URL.RawQuery)
		ms := s.mergeStatus
		mp := s.mergePR
		s.mu.Unlock()
		if ms == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(db.PendingMerge{
			PR:          mp,
			SessionName: "test",
			InstanceID:  "inst",
			Status:      ms,
			QueuedAt:    time.Now(),
		})
	})
	mux.HandleFunc("/sessions/status", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, "/sessions/status?"+r.URL.RawQuery)
		ss := s.sessionState
		sn := s.sessionName
		s.mu.Unlock()
		if ss == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(db.Status{SessionName: sn, State: ss, Repo: "repo", Worktree: "/wt"})
	})
	mux.HandleFunc("/groups/poll", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, "/groups/poll?"+r.URL.RawQuery)
		completed := s.groupCompleted
		members := s.groupMembers
		results := s.groupResults
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"completed": completed,
			"members":   members,
			"results":   results,
		})
	})
	return mux
}

// startFakeWaitProbeServer launches the fake server on a unix socket and
// returns the apiURL. The unix-socket path is kept short for the same
// macOS sun_path-104 reason documented in merge_proxy_test.go.
func startFakeWaitProbeServer(t *testing.T) (*fakeWaitProbeServer, string) {
	t.Helper()
	sockDir, err := os.MkdirTemp("/tmp", "wp")
	if err != nil {
		t.Fatalf("mkdir short sock dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sockPath := filepath.Join(sockDir, "h.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}
	s := &fakeWaitProbeServer{}
	srv := &http.Server{Handler: s.handler()}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return s, "unix://" + sockPath
}

// TestObserveAlreadyTerminal_RoutesViaHostAPI is the headline regression
// test for the review-code finding: when PRISM_HOST_API is set, the wait
// probes must hit the host-API socket instead of the local DB. Without
// the proxy probe path, this test would fail because the fake server's
// "merged" response is invisible to a shadow-DB read.
func TestObserveAlreadyTerminal_RoutesViaHostAPI(t *testing.T) {
	openMergeTestDB(t) // sets PRISM_HOST_API="" — we override below.

	server, apiURL := startFakeWaitProbeServer(t)
	server.mu.Lock()
	server.mergePR = 99
	server.mergeStatus = "merged"
	server.mu.Unlock()
	t.Setenv("PRISM_HOST_API", apiURL)

	out := captureStdout(t, func() {
		done, err := observeAlreadyTerminal(99, false)
		if !done {
			t.Fatal("expected proxy probe to short-circuit on merged row")
		}
		if err != nil {
			t.Errorf("expected nil error on merged, got %v", err)
		}
	})
	if !strings.Contains(out, "PR #99 merged") {
		t.Errorf("expected merged summary, got %q", out)
	}

	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.requests) == 0 {
		t.Fatal("server received no requests — probe did not route via host-API")
	}
	if !strings.Contains(server.requests[0], "/merges/by-pr") {
		t.Errorf("first request was %q, expected /merges/by-pr", server.requests[0])
	}
}

// TestWaitForSpawnTerminal_RoutesViaHostAPI exercises the proxy path for
// spawn --wait. Without the proxy probe the in-sandbox caller would poll
// the shadow DB and never see the host's `finished` state — silently
// timing out instead of returning 0.
func TestWaitForSpawnTerminal_RoutesViaHostAPI(t *testing.T) {
	openMergeTestDB(t) // sets PRISM_HOST_API="" then we override.

	server, apiURL := startFakeWaitProbeServer(t)
	server.mu.Lock()
	server.sessionName = "repo@feature"
	server.sessionState = "finished"
	server.mu.Unlock()
	t.Setenv("PRISM_HOST_API", apiURL)

	out := captureStdout(t, func() {
		if err := waitForSpawnTerminal("repo@feature", false, 5*time.Second); err != nil {
			t.Errorf("waitForSpawnTerminal via proxy: expected nil on finished, got %v", err)
		}
	})
	if !strings.Contains(out, "finished") {
		t.Errorf("expected finished summary, got %q", out)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	hits := 0
	for _, r := range server.requests {
		if strings.HasPrefix(r, "/sessions/status") {
			hits++
		}
	}
	if hits == 0 {
		t.Errorf("expected at least one /sessions/status request, got %v", server.requests)
	}
}

// TestWaitForReviewTerminal_RoutesViaHostAPI exercises the proxy path for
// review --wait. The fake server reports the group as completed with one
// finished, PASS-marker member; the wait helper must aggregate via the
// proxy probe and return nil (exit 0).
func TestWaitForReviewTerminal_RoutesViaHostAPI(t *testing.T) {
	openMergeTestDB(t)

	server, apiURL := startFakeWaitProbeServer(t)
	server.mu.Lock()
	server.groupID = "g-1"
	server.groupCompleted = true
	server.groupMembers = []db.Status{{SessionName: "repo@pr-1500~review-1-review-goal", State: "finished", Repo: "repo", Worktree: "/wt"}}
	server.groupResults = map[string]db.GroupMemberResult{
		"repo@pr-1500~review-1-review-goal": {
			SessionName: "repo@pr-1500~review-1-review-goal",
			State:       "finished",
			LastMessage: `{"text":"<verdict>PASS</verdict> looks good"}`,
		},
	}
	server.mu.Unlock()
	t.Setenv("PRISM_HOST_API", apiURL)

	out := captureStdout(t, func() {
		if err := waitForReviewTerminal("1500", "g-1", false, 5*time.Second); err != nil {
			t.Errorf("waitForReviewTerminal via proxy: expected nil on PASS, got %v", err)
		}
	})
	if !strings.Contains(out, "verdict: PASS") {
		t.Errorf("expected PASS summary, got %q", out)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	hits := 0
	for _, r := range server.requests {
		if strings.HasPrefix(r, "/groups/poll") {
			hits++
		}
	}
	if hits == 0 {
		t.Errorf("expected at least one /groups/poll request, got %v", server.requests)
	}
}

// TestParseReviewGroupIDFromAck verifies the regex-based extraction used
// by the in-sandbox `prism review --wait` proxy path.
func TestParseReviewGroupIDFromAck(t *testing.T) {
	cases := []struct {
		ack  string
		want string
	}{
		{"Review in progress — PR #1500, round 1 (group: 5103f218-9e18-423c-a298-2448dac6ab26)\n\nMore output here", "5103f218-9e18-423c-a298-2448dac6ab26"},
		{"no group line here", ""},
		{"(group: not-a-uuid)", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := parseReviewGroupIDFromAck(tc.ack); got != tc.want {
			t.Errorf("parseReviewGroupIDFromAck(%q): got %q, want %q", tc.ack, got, tc.want)
		}
	}
}
