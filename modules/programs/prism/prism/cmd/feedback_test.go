package cmd

// Tests for `prism feedback`. All tests redirect the store path to a
// t.TempDir() via XDG_STATE_HOME so they pass inside the nix-build
// sandbox where HOME=/homeless-shelter is unwritable.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/feedback"
)

// withTempFeedbackStore points the feedback store at a tempdir and clears
// PRISM_FEEDBACK_ENDPOINT and PRISM_HOST_API so tests don't accidentally use
// the real endpoint or proxy through the host-API of the developer's live
// prism session.
// Returns the on-disk path so individual tests can inspect the JSONL.
func withTempFeedbackStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv(PRISMFeedbackEndpointEnv, "")
	// Clear PRISM_HOST_API so tests that exercise the local-write path are not
	// accidentally routed through the sandbox proxy (which would happen when the
	// test binary runs inside a prism worker session where PRISM_HOST_API is set).
	t.Setenv("PRISM_HOST_API", "")
	return filepath.Join(dir, "prism", "feedback.jsonl")
}

// buildFreshFeedbackTree returns a freshly-constructed `feedback` cobra
// subtree so flag state from one test does not leak into another. The
// real production tree (cmd.feedbackCmd / feedbackListCmd / feedbackPruneCmd)
// is a package singleton; building a parallel one for tests sidesteps that.
func buildFreshFeedbackTree(t *testing.T) *cobra.Command {
	t.Helper()
	root := &cobra.Command{Use: "prism", SilenceUsage: true, SilenceErrors: true}

	rec := &cobra.Command{
		Use:          "feedback",
		Args:         cobra.MaximumNArgs(1),
		RunE:         runFeedbackRecord,
		SilenceUsage: true,
	}

	list := &cobra.Command{Use: "list", Args: cobra.NoArgs, RunE: runFeedbackList, SilenceUsage: true}
	list.Flags().Bool("json", false, "")
	list.Flags().Int("days", 0, "")

	prune := &cobra.Command{Use: "prune", Args: cobra.NoArgs, RunE: runFeedbackPrune, SilenceUsage: true}
	prune.Flags().Int("days", 0, "")
	prune.Flags().Bool("yes", false, "")

	rec.AddCommand(list, prune)
	root.AddCommand(rec)
	return root
}

// runFeedbackCmd executes the test-only feedback tree with the given args.
// Returns combined stdout+stderr and the RunE error.
func runFeedbackCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := buildFreshFeedbackTree(t)
	root.SetArgs(append([]string{"feedback"}, args...))
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	err := root.Execute()
	return buf.String(), err
}

// ── record ──────────────────────────────────────────────────────────────────

func TestFeedback_Record_AppendsLocallyAndPrintsConfirmation(t *testing.T) {
	storePath := withTempFeedbackStore(t)

	out, err := runFeedbackCmd(t, "the --tier flag rejects 'enterprise'")
	if err != nil {
		t.Fatalf("feedback: %v\nout=%s", err, out)
	}
	if !strings.Contains(out, "feedback recorded locally") {
		t.Errorf("output missing confirmation: %q", out)
	}

	data, readErr := os.ReadFile(storePath)
	if readErr != nil {
		t.Fatalf("ReadFile %s: %v", storePath, readErr)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %q", len(lines), data)
	}
	var e feedback.Entry
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil {
		t.Fatalf("parse entry: %v (raw: %s)", err, lines[0])
	}
	if e.Text != "the --tier flag rejects 'enterprise'" {
		t.Errorf("text = %q", e.Text)
	}
	if e.Timestamp == "" {
		t.Errorf("timestamp is empty")
	}
	if _, err := time.Parse(time.RFC3339, e.Timestamp); err != nil {
		t.Errorf("timestamp not RFC3339: %v", err)
	}
}

// AC edge-case: invoked with no argument prism feedback should refuse with
// a clear usage hint and a non-zero exit.
func TestFeedback_Record_NoArgErrors(t *testing.T) {
	withTempFeedbackStore(t)

	out, err := runFeedbackCmd(t)
	if err == nil {
		t.Fatalf("expected error for no-arg invocation, got out=%q", out)
	}
	if !strings.Contains(err.Error(), "feedback text is required") {
		t.Errorf("err = %v; want 'feedback text is required'", err)
	}
}

// AC: prism feedback - reads from stdin.
func TestFeedback_Record_StdinDash(t *testing.T) {
	withTempFeedbackStore(t)

	stdin := strings.NewReader("piped feedback text\n")
	got, err := readFeedbackText([]string{"-"}, stdin)
	if err != nil {
		t.Fatalf("readFeedbackText: %v", err)
	}
	if got != "piped feedback text" {
		t.Errorf("got %q", got)
	}
}

func TestFeedback_Record_StdinDash_EmptyErrors(t *testing.T) {
	withTempFeedbackStore(t)
	_, err := readFeedbackText([]string{"-"}, strings.NewReader(""))
	if err == nil {
		t.Fatal("expected error for empty stdin")
	}
}

// AC: when PRISM_FEEDBACK_ENDPOINT is set, feedback is also POSTed upstream
// after being recorded locally. The local record is unaffected by upstream
// failure; HTTP status is reported in the success message.
func TestFeedback_Record_PostsUpstreamWhenConfigured(t *testing.T) {
	storePath := withTempFeedbackStore(t)

	var (
		gotMethod      string
		gotContentType string
		gotBody        []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv(PRISMFeedbackEndpointEnv, srv.URL)

	out, err := runFeedbackCmd(t, "test note")
	if err != nil {
		t.Fatalf("feedback: %v\nout=%s", err, out)
	}
	if !strings.Contains(out, "sent upstream") || !strings.Contains(out, "200") {
		t.Errorf("output missing upstream confirmation: %q", out)
	}
	if gotMethod != "POST" {
		t.Errorf("method = %q", gotMethod)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q", gotContentType)
	}
	var posted feedback.Entry
	if err := json.Unmarshal(gotBody, &posted); err != nil {
		t.Errorf("posted body not JSON: %v (raw: %s)", err, gotBody)
	}
	if posted.Text != "test note" {
		t.Errorf("posted text = %q", posted.Text)
	}

	data, _ := os.ReadFile(storePath)
	if !strings.Contains(string(data), "test note") {
		t.Errorf("local record missing: %q", data)
	}
}

func TestFeedback_Record_UpstreamFailureDoesNotLoseLocal(t *testing.T) {
	storePath := withTempFeedbackStore(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv(PRISMFeedbackEndpointEnv, srv.URL)

	out, err := runFeedbackCmd(t, "test note")
	if err != nil {
		t.Fatalf("feedback should not fail on upstream 500, got: %v", err)
	}
	if !strings.Contains(out, "upstream POST failed") {
		t.Errorf("output should report upstream failure: %q", out)
	}
	data, _ := os.ReadFile(storePath)
	if !strings.Contains(string(data), "test note") {
		t.Errorf("local record missing despite upstream failure: %q", data)
	}
}

// ── list ────────────────────────────────────────────────────────────────────

func TestFeedback_List_HumanReadable(t *testing.T) {
	withTempFeedbackStore(t)
	if _, err := runFeedbackCmd(t, "first note"); err != nil {
		t.Fatal(err)
	}
	if _, err := runFeedbackCmd(t, "second note"); err != nil {
		t.Fatal(err)
	}
	out, err := runFeedbackCmd(t, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "first note") || !strings.Contains(out, "second note") {
		t.Errorf("list output missing entries: %q", out)
	}
}

func TestFeedback_List_JSON(t *testing.T) {
	withTempFeedbackStore(t)
	if _, err := runFeedbackCmd(t, "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := runFeedbackCmd(t, "beta"); err != nil {
		t.Fatal(err)
	}
	out, err := runFeedbackCmd(t, "list", "--json")
	if err != nil {
		t.Fatalf("list --json: %v", err)
	}
	var entries []feedback.Entry
	if jerr := json.Unmarshal([]byte(out), &entries); jerr != nil {
		t.Fatalf("not JSON: %v\nout=%s", jerr, out)
	}
	if len(entries) != 2 {
		t.Errorf("got %d entries, want 2", len(entries))
	}
}

func TestFeedback_List_JSON_EmptyIsArrayNotNull(t *testing.T) {
	withTempFeedbackStore(t)
	out, err := runFeedbackCmd(t, "list", "--json")
	if err != nil {
		t.Fatalf("list --json: %v", err)
	}
	trimmed := strings.TrimSpace(out)
	if trimmed != "[]" {
		t.Errorf("empty list --json should be []; got %q", trimmed)
	}
}

func TestFeedback_List_DaysFilter(t *testing.T) {
	storePath := withTempFeedbackStore(t)

	old := feedback.Entry{
		Timestamp:    time.Now().Add(-30 * 24 * time.Hour).Format(time.RFC3339),
		Text:         "old note",
		PrismVersion: "v",
	}
	recent := feedback.Entry{
		Timestamp:    time.Now().Add(-1 * time.Hour).Format(time.RFC3339),
		Text:         "recent note",
		PrismVersion: "v",
	}
	if err := os.MkdirAll(filepath.Dir(storePath), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(storePath)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	_ = enc.Encode(old)
	_ = enc.Encode(recent)
	_ = f.Close()

	out, err := runFeedbackCmd(t, "list", "--days", "7")
	if err != nil {
		t.Fatalf("list --days 7: %v", err)
	}
	if !strings.Contains(out, "recent note") {
		t.Errorf("output missing recent note: %q", out)
	}
	if strings.Contains(out, "old note") {
		t.Errorf("output unexpectedly contains old note: %q", out)
	}
}

// ── prune ───────────────────────────────────────────────────────────────────

// AC: prune without --yes errors instead of prompting (principle 1).
func TestFeedback_Prune_RequiresYes(t *testing.T) {
	withTempFeedbackStore(t)
	out, err := runFeedbackCmd(t, "prune", "--days", "30")
	if err == nil {
		t.Fatalf("expected error without --yes; got out=%q", out)
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("err = %v; want --yes-required message", err)
	}
}

func TestFeedback_Prune_RequiresPositiveDays(t *testing.T) {
	withTempFeedbackStore(t)
	out, err := runFeedbackCmd(t, "prune", "--yes")
	if err == nil {
		t.Fatalf("expected error without --days; got out=%q", out)
	}
	if !strings.Contains(err.Error(), "--days") {
		t.Errorf("err = %v; want --days-required message", err)
	}
}

func TestFeedback_Prune_DropsOldEntries(t *testing.T) {
	storePath := withTempFeedbackStore(t)

	if err := os.MkdirAll(filepath.Dir(storePath), 0o755); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Create(storePath)
	enc := json.NewEncoder(f)
	_ = enc.Encode(feedback.Entry{
		Timestamp:    time.Now().Add(-60 * 24 * time.Hour).Format(time.RFC3339),
		Text:         "old",
		PrismVersion: "v",
	})
	_ = enc.Encode(feedback.Entry{
		Timestamp:    time.Now().Add(-1 * time.Hour).Format(time.RFC3339),
		Text:         "recent",
		PrismVersion: "v",
	})
	_ = f.Close()

	out, err := runFeedbackCmd(t, "prune", "--days", "30", "--yes")
	if err != nil {
		t.Fatalf("prune: %v\nout=%s", err, out)
	}
	if !strings.Contains(out, "removed 1") {
		t.Errorf("output missing removed-1: %q", out)
	}

	data, _ := os.ReadFile(storePath)
	if strings.Contains(string(data), `"text":"old"`) {
		t.Errorf("old entry not pruned: %s", data)
	}
	if !strings.Contains(string(data), `"text":"recent"`) {
		t.Errorf("recent entry pruned by mistake: %s", data)
	}
}

// AC: feedback entry includes the calling session name when
// PRISM_SESSION_NAME is set.
func TestFeedback_BuildEntry_IncludesSessionFromEnv(t *testing.T) {
	t.Setenv("PRISM_SESSION_NAME", "myrepo@feature")
	e := buildFeedbackEntry("hi", time.Now().UTC())
	if e.Session != "myrepo@feature" {
		t.Errorf("session = %q, want myrepo@feature", e.Session)
	}
}

func TestFeedback_BuildEntry_OmitsSessionWhenUnset(t *testing.T) {
	t.Setenv("PRISM_SESSION_NAME", "")
	e := buildFeedbackEntry("hi", time.Now().UTC())
	if e.Session != "" {
		t.Errorf("session = %q, want empty", e.Session)
	}
}

// AC: when feedback_endpoint is set in the config file (and
// PRISM_FEEDBACK_ENDPOINT is unset), the upstream POST is still attempted.
func TestFeedback_Record_ReadsEndpointFromConfigFile(t *testing.T) {
	storePath := withTempFeedbackStore(t)

	var gotPosted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPosted = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Write a config file with feedback_endpoint set.
	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "config.json")
	cfg := []byte(`{"feedback_endpoint":"` + srv.URL + `"}`)
	if err := os.WriteFile(cfgPath, cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRISM_CONFIG_FILE", cfgPath)
	t.Setenv(PRISMFeedbackEndpointEnv, "") // env unset; config wins

	out, err := runFeedbackCmd(t, "config-key note")
	if err != nil {
		t.Fatalf("feedback: %v\nout=%s", err, out)
	}
	if !gotPosted {
		t.Errorf("upstream was not POSTed despite config feedback_endpoint")
	}
	if !strings.Contains(out, "sent upstream") {
		t.Errorf("output missing 'sent upstream': %q", out)
	}
	if _, statErr := os.Stat(storePath); statErr != nil {
		t.Errorf("local store missing: %v", statErr)
	}
}

// ── host-API proxy path (PRISM_HOST_API) ────────────────────────────────────

// TestFeedback_Record_HostAPIProxy_Success verifies that when PRISM_HOST_API
// is set, the entry is POSTed to the host-API /feedback endpoint and the
// success message shows the path returned by the server. Nothing is written
// to the local store (the current process is inside a sandbox and the local
// path is ephemeral).
func TestFeedback_Record_HostAPIProxy_Success(t *testing.T) {
	// Point the local store to a tempdir so we can verify nothing was written
	// locally by the proxy path.
	local := withTempFeedbackStore(t)

	var (
		gotMethod string
		gotBody   []byte
	)
	hostPath := "/host/state/prism/feedback.jsonl"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"path":"` + hostPath + `"}`))
	}))
	defer srv.Close()
	t.Setenv("PRISM_HOST_API", srv.URL)

	out, err := runFeedbackCmd(t, "sandbox note")
	if err != nil {
		t.Fatalf("feedback via host-API: %v\nout=%s", err, out)
	}

	// Success message must show the host path (not the sandbox-local path).
	if !strings.Contains(out, "host-API") {
		t.Errorf("output missing 'host-API': %q", out)
	}
	if !strings.Contains(out, hostPath) {
		t.Errorf("output missing host path %q: %q", hostPath, out)
	}

	// The entry must have been POSTed as JSON.
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	var posted feedback.Entry
	if err := json.Unmarshal(gotBody, &posted); err != nil {
		t.Errorf("posted body not valid JSON: %v (raw: %s)", err, gotBody)
	}
	if posted.Text != "sandbox note" {
		t.Errorf("posted text = %q, want 'sandbox note'", posted.Text)
	}

	// Nothing must have been written locally — the sandbox write path is the
	// bug this fix resolves.
	if _, statErr := os.Stat(local); statErr == nil {
		data, _ := os.ReadFile(local)
		t.Errorf("data unexpectedly written to local store: %s", data)
	}
}

// TestFeedback_Record_HostAPIProxy_ErrorExitsNonZero verifies that when
// PRISM_HOST_API is set but the host-API returns an error, the CLI exits
// non-zero and does NOT silently write to the sandbox-internal path.
func TestFeedback_Record_HostAPIProxy_ErrorExitsNonZero(t *testing.T) {
	local := withTempFeedbackStore(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"append feedback: disk full"}`))
	}))
	defer srv.Close()
	t.Setenv("PRISM_HOST_API", srv.URL)

	_, err := runFeedbackCmd(t, "will fail")
	if err == nil {
		t.Fatalf("expected non-zero exit when host-API returns 500, got nil")
	}
	if !strings.Contains(err.Error(), "host-API") {
		t.Errorf("error %v: want mention of 'host-API'", err)
	}

	// Crucially: nothing written locally (no silent fallback).
	if _, statErr := os.Stat(local); statErr == nil {
		data, _ := os.ReadFile(local)
		t.Errorf("data written locally despite host-API failure: %s", data)
	}
}

// TestFeedback_Record_HostAPIProxy_UnreachableExitsNonZero verifies that when
// PRISM_HOST_API is set but the socket is missing/unresponsive, the CLI exits
// non-zero and does NOT silently write to the sandbox-internal path.
func TestFeedback_Record_HostAPIProxy_UnreachableExitsNonZero(t *testing.T) {
	local := withTempFeedbackStore(t)

	// Point to a URL where nothing is listening.
	t.Setenv("PRISM_HOST_API", "http://127.0.0.1:19999")

	_, err := runFeedbackCmd(t, "unreachable")
	if err == nil {
		t.Fatalf("expected non-zero exit when host-API is unreachable, got nil")
	}

	// Nothing written locally.
	if _, statErr := os.Stat(local); statErr == nil {
		data, _ := os.ReadFile(local)
		t.Errorf("data written locally despite unreachable host-API: %s", data)
	}
}

// TestFeedback_Record_NoHostAPI_WritesLocally verifies that when PRISM_HOST_API
// is NOT set (running from a host shell, not inside a sandbox), the existing
// local-write path applies unchanged — no regression.
func TestFeedback_Record_NoHostAPI_WritesLocally(t *testing.T) {
	storePath := withTempFeedbackStore(t)
	t.Setenv("PRISM_HOST_API", "") // explicitly unset

	out, err := runFeedbackCmd(t, "host note")
	if err != nil {
		t.Fatalf("feedback: %v\nout=%s", err, out)
	}
	if !strings.Contains(out, "feedback recorded locally") {
		t.Errorf("output missing 'feedback recorded locally': %q", out)
	}
	data, readErr := os.ReadFile(storePath)
	if readErr != nil {
		t.Fatalf("ReadFile %s: %v", storePath, readErr)
	}
	if !strings.Contains(string(data), "host note") {
		t.Errorf("local store missing entry: %s", data)
	}
}

// AC: PRISM_FEEDBACK_ENDPOINT (env) takes precedence over the config key.
func TestFeedback_Record_EnvOverridesConfigEndpoint(t *testing.T) {
	withTempFeedbackStore(t)

	var (
		envHit    bool
		configHit bool
	)
	envSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		envHit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer envSrv.Close()
	cfgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		configHit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer cfgSrv.Close()

	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "config.json")
	cfg := []byte(`{"feedback_endpoint":"` + cfgSrv.URL + `"}`)
	_ = os.WriteFile(cfgPath, cfg, 0o644)
	t.Setenv("PRISM_CONFIG_FILE", cfgPath)
	t.Setenv(PRISMFeedbackEndpointEnv, envSrv.URL)

	if _, err := runFeedbackCmd(t, "x"); err != nil {
		t.Fatal(err)
	}
	if !envHit {
		t.Errorf("env endpoint should have been hit")
	}
	if configHit {
		t.Errorf("config endpoint should NOT have been hit when env is set")
	}
}
