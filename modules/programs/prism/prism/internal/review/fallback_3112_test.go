package review_test

// fallback_3112_test.go — issue #3112. A review agent that ends with a
// run_error and a provider refusal runs again on the next fallback model of
// its profile slot, in the same round. A real refusal cannot be forced, so
// these tests seed a run_error event in place of the frame the extension
// sends.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
)

const (
	fbParent   = "nixos-config@fallback-3112"
	fbWorker   = "nixos-config@fallback-3112-worker"
	fbFirst    = "anthropic/claude-opus-5-5"
	fbModelOne = "anthropic/claude-opus-4-8"
	fbModelTwo = "anthropic/claude-sonnet-5-5"
	fbPrompt   = "review PR 3112 please"
)

// seedRunError writes the run_error event the sidecar stores for a failed
// turn, and puts the session in state "error".
func seedRunError(t *testing.T, d *db.DB, sess, raw string) {
	t.Helper()
	payload := map[string]string{
		"type":        "run_error",
		"stop_reason": "error",
		"reason":      `the model ended its turn with no text and no tool call (stop reason "error", provider stop reason "` + raw + `")`,
	}
	if raw != "" {
		payload["raw_stop_reason"] = raw
	}
	b, _ := json.Marshal(payload)
	if err := d.WriteEvent(db.Event{
		ID: sess + "-run-error", SessionName: sess, Repo: "nixos-config", Worktree: "/wt",
		Type: "run_error", Payload: string(b),
	}); err != nil {
		t.Fatalf("WriteEvent run_error: %v", err)
	}
	if err := d.UpsertStatus(sess, "nixos-config", "/wt", "error", nil, nil); err != nil {
		t.Fatalf("UpsertStatus error: %v", err)
	}
}

func seedMember(t *testing.T, d *db.DB, groupID, sess, state string) {
	t.Helper()
	if err := d.UpsertStatus(sess, "nixos-config", "/wt", state, nil, nil); err != nil {
		t.Fatalf("UpsertStatus(%q): %v", sess, err)
	}
	if err := d.SetGroupID(sess, groupID); err != nil {
		t.Fatalf("SetGroupID(%q): %v", sess, err)
	}
}

// fallbackHarness seeds a round and fakes the fallback spawns. outcomes[n]
// is what the n-th fallback session does: "" passes, and any other value is
// the raw stop reason of its run_error.
type fallbackHarness struct {
	t        *testing.T
	d        *db.DB
	groupID  string
	sessions []string
	agents   []review.Agent
	mu       sync.Mutex
	calls    []review.FallbackSpawnCall
	outcomes []string
	text     string
}

// newFallbackHarness seeds a round of two agents: review-goal passes, and
// review-security ends with a run_error whose raw stop reason is firstRaw.
func newFallbackHarness(t *testing.T, firstRaw string, outcomes ...string) *fallbackHarness {
	d := openTestDB(t)
	groupID, err := d.RegisterGroup(fbParent)
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}
	h := &fallbackHarness{
		t: t, d: d, groupID: groupID, outcomes: outcomes,
		agents:   []review.Agent{{Name: "review-goal"}, {Name: "review-security"}},
		sessions: []string{fbParent + "~review-1-review-goal", fbParent + "~review-1-review-security"},
	}
	seedMember(t, d, groupID, h.sessions[0], "finished")
	seedAssistantEvent(t, d, h.sessions[0], "<summary>ok</summary>\n<verdict>PASS</verdict>")
	seedMember(t, d, groupID, h.sessions[1], "active")
	seedRunError(t, d, h.sessions[1], firstRaw)
	return h
}

func (h *fallbackHarness) spawn(d *db.DB, c review.FallbackSpawnCall) error {
	h.mu.Lock()
	n := len(h.calls)
	h.calls = append(h.calls, c)
	h.mu.Unlock()
	seedMember(h.t, d, c.GroupID, c.Session, "active")
	if n < len(h.outcomes) && h.outcomes[n] != "" {
		seedRunError(h.t, d, c.Session, h.outcomes[n])
		return nil
	}
	seedAssistantEvent(h.t, d, c.Session, "<summary>fine</summary>\n<verdict>PASS</verdict>")
	if err := d.UpsertStatus(c.Session, "nixos-config", "/wt", "finished", nil, nil); err != nil {
		h.t.Fatalf("UpsertStatus finished: %v", err)
	}
	return nil
}

// run drives MonitorFunc to delivery and returns the delivered text.
func (h *fallbackHarness) run(fallbacks []string) string {
	t := h.t
	if err := h.d.UpsertStatus(fbWorker, "nixos-config", "/wt", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus worker: %v", err)
	}
	var delivered string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil && len(body.Parts) > 0 {
			delivered = body.Parts[0].Text
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	if err := setHarnessInfo(t, h.d, fbWorker, extractPort(t, srv.URL), "sid"); err != nil {
		t.Fatalf("setHarnessInfo: %v", err)
	}

	opts := review.MonitorOpts{
		GroupID:              h.groupID,
		WorkerSession:        fbWorker,
		PRNumber:             "3112",
		Round:                1,
		Agents:               h.agents,
		AgentSessions:        h.sessions,
		DBPath:               h.d.Path(),
		PollInterval:         10 * time.Millisecond,
		DeliveryRetryBackoff: time.Millisecond,
		Timeout:              10 * time.Second,
		Fallback: &review.FallbackPlan{
			ProfileName: "max",
			Agents: []review.AgentFallbackPlan{
				{Prompt: "goal prompt", Model: fbFirst},
				{Prompt: fbPrompt, Model: fbFirst, Fallbacks: fallbacks},
			},
		},
	}
	review.SetFallbackSpawnerForTest(&opts, h.spawn)
	if err := review.MonitorFunc(opts); err != nil {
		t.Fatalf("MonitorFunc: %v", err)
	}
	if delivered == "" {
		t.Fatal("no review-complete text delivered")
	}
	return delivered
}

func TestFallback_RefusalRunsFirstFallbackInSameRound(t *testing.T) {
	h := newFallbackHarness(t, "refusal")
	text := h.run([]string{fbModelOne, fbModelTwo})

	if len(h.calls) != 1 {
		t.Fatalf("fallback spawns = %d, want 1", len(h.calls))
	}
	c := h.calls[0]
	if c.Session != h.sessions[1]+"-fallback-1" {
		t.Errorf("Session = %q, want %q", c.Session, h.sessions[1]+"-fallback-1")
	}
	if c.AgentName != "review-security" || c.Prompt != fbPrompt || c.GroupID != h.groupID || c.ProfileName != "max" {
		t.Errorf("spawn call = %+v, want the same agent, prompt, group, and profile", c)
	}
	if got := c.ModelsByRole["review-security"]; got != fbModelOne {
		t.Errorf("model = %q, want %q", got, fbModelOne)
	}
	if strings.Contains(text, "✗") {
		t.Errorf("round did not pass after the fallback:\n%s", text)
	}
	for _, want := range []string{
		"**Model:** " + fbModelOne,
		"**Model:** " + fbFirst,
		fbFirst + ` stopped with provider stop reason "refusal". Fallback: ` + fbModelOne,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("report does not contain %q:\n%s", want, text)
		}
	}
}

func TestFallback_SensitiveThenRefusalRunsNextFallback(t *testing.T) {
	h := newFallbackHarness(t, "sensitive", "refusal")
	text := h.run([]string{fbModelOne, fbModelTwo})

	if len(h.calls) != 2 {
		t.Fatalf("fallback spawns = %d, want 2", len(h.calls))
	}
	if got := h.calls[1].ModelsByRole["review-security"]; got != fbModelTwo {
		t.Errorf("second model = %q, want %q", got, fbModelTwo)
	}
	if h.calls[1].Session != h.sessions[1]+"-fallback-2" {
		t.Errorf("second session = %q", h.calls[1].Session)
	}
	for _, want := range []string{
		"**Model:** " + fbModelTwo,
		`stop reason "sensitive". Fallback: ` + fbModelOne,
		fbModelOne + ` stopped with provider stop reason "refusal". Fallback: ` + fbModelTwo,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("report does not contain %q:\n%s", want, text)
		}
	}
}

func TestFallback_LastFallbackRefusesEndsWithRunError(t *testing.T) {
	h := newFallbackHarness(t, "refusal", "refusal", "refusal")
	text := h.run([]string{fbModelOne, fbModelTwo})

	if len(h.calls) != 2 {
		t.Fatalf("fallback spawns = %d, want 2 (one per fallback model)", len(h.calls))
	}
	if !strings.Contains(text, "did not complete cleanly") || !strings.Contains(text, `provider stop reason "refusal"`) {
		t.Errorf("report does not show the run_error of the last fallback:\n%s", text)
	}
	if !strings.Contains(text, "**Model:** "+fbModelTwo) {
		t.Errorf("report does not name the last model:\n%s", text)
	}
}

func TestFallback_OtherStopReasonDoesNotStartFallback(t *testing.T) {
	for _, raw := range []string{"", "max_tokens", "brand_new_reason"} {
		t.Run("raw="+raw, func(t *testing.T) {
			h := newFallbackHarness(t, raw)
			text := h.run([]string{fbModelOne, fbModelTwo})
			if len(h.calls) != 0 {
				t.Fatalf("fallback spawns = %d, want 0", len(h.calls))
			}
			if strings.Contains(text, "**Fallbacks:**") {
				t.Errorf("report lists a fallback:\n%s", text)
			}
		})
	}
}

func TestFallback_RoundWithFallbackCountsAsOneCycle(t *testing.T) {
	h := newFallbackHarness(t, "refusal")
	// FAIL on review-goal, so the round is a verdict-producing failure.
	if err := h.d.WriteEvent(db.Event{
		ID: h.sessions[0] + "-evt-2", SessionName: h.sessions[0], Repo: "nixos-config", Worktree: "/wt",
		Type: "msg_assistant", Payload: `{"text":"<verdict>FAIL</verdict>"}`,
	}); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	h.run([]string{fbModelOne})

	n, err := review.CompletedReviewCyclesForParent(h.d, fbParent, "")
	if err != nil {
		t.Fatalf("CompletedReviewCyclesForParent: %v", err)
	}
	if n != 1 {
		t.Errorf("cycles = %d, want 1", n)
	}
}

func TestBuildFallbackPlan_ModelOverrideDisablesFallback(t *testing.T) {
	pf := &config.ProfilesFile{Profiles: map[string]config.ProfileEntry{
		"max": {
			"review-security": {Model: fbFirst, Fallbacks: []string{fbModelOne, fbModelTwo}},
			"review-goal":     {Model: fbFirst},
		},
	}}
	agents := []review.Agent{{Name: "review-goal"}, {Name: "review-security"}}

	got := review.BuildFallbackPlanForTest(pf, "max", nil, agents, []string{"a", "b"})
	if !reflect.DeepEqual(got[1].Fallbacks, []string{fbModelOne, fbModelTwo}) || got[1].Model != fbFirst || got[1].Prompt != "b" {
		t.Errorf("plan without override = %+v", got[1])
	}
	if len(got[0].Fallbacks) != 0 {
		t.Errorf("review-goal fallbacks = %v, want none", got[0].Fallbacks)
	}

	got = review.BuildFallbackPlanForTest(pf, "max", map[string]string{"review-security": fbModelTwo}, agents, []string{"a", "b"})
	if len(got[1].Fallbacks) != 0 {
		t.Errorf("fallbacks with --model-override = %v, want none", got[1].Fallbacks)
	}
	if got[1].Model != fbModelTwo {
		t.Errorf("model with --model-override = %q, want %q", got[1].Model, fbModelTwo)
	}
}

func TestEffectiveRoundSessions_CollapsesFallbackChain(t *testing.T) {
	in := []string{
		"p~review-1-review-security-fallback-2",
		"p~review-1-review-goal",
		"p~review-1-review-security",
		"p~review-1-review-security-fallback-1",
	}
	want := []string{"p~review-1-review-security-fallback-2", "p~review-1-review-goal"}
	if got := review.EffectiveRoundSessions(in); !reflect.DeepEqual(got, want) {
		t.Errorf("EffectiveRoundSessions = %v, want %v", got, want)
	}
}

// TestFallbackProfiles_LoadsWhenNil covers host mode: cmd/review.go does not
// load profiles.json for it, so the fallback plan loads the file itself.
func TestFallbackProfiles_LoadsWhenNil(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	if err := os.MkdirAll(filepath.Join(cfg, "prism"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"profiles":{"max":{"review-security":{"model":"` + fbFirst + `","fallbacks":["` + fbModelOne + `"]}}}}`
	if err := os.WriteFile(filepath.Join(cfg, "prism", "profiles.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	pf := review.FallbackProfilesForTest(nil)
	plan := review.BuildFallbackPlanForTest(pf, "max", nil, []review.Agent{{Name: "review-security"}}, []string{"p"})
	if !reflect.DeepEqual(plan[0].Fallbacks, []string{fbModelOne}) || plan[0].Model != fbFirst {
		t.Errorf("plan = %+v, want the slot model and fallbacks from profiles.json", plan[0])
	}

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if got := review.FallbackProfilesForTest(nil); got != nil {
		t.Errorf("missing profiles.json: got %+v, want nil", got)
	}
}
