package mergequeue

// Issue #3282: a stop of the coordinator sidecar after the watcher wrote a
// terminal state.
//
// The stop cancels the context of the watcher. The watcher used that
// context for the notification request on the HTTP harness path, so the
// cancel stopped the request, and the coordinator did not get the outcome.
// Now the row holds the notification until a watcher sends it. The request
// continues for a short time after the cancel. If it fails while the
// watcher stops, the notification goes back to the row, and the next
// watcher sends it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/harness"
	"github.com/prismatic-koi/prism/internal/session"
)

// fakeHarness is the HTTP harness of the coordinator. It records the text
// of each prompt_async request. answer, when set, runs in the request and
// gives the status code. The default is 200.
type fakeHarness struct {
	*httptest.Server
	mu     sync.Mutex
	texts  []string
	answer func(r *http.Request) int
}

func newFakeHarness(t *testing.T, answer func(r *http.Request) int) *fakeHarness {
	t.Helper()
	h := &fakeHarness{answer: answer}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		}
		_ = json.Unmarshal(body, &parsed)
		var text string
		if len(parsed.Parts) > 0 {
			text = parsed.Parts[0].Text
		}
		h.mu.Lock()
		h.texts = append(h.texts, text)
		h.mu.Unlock()
		code := http.StatusOK
		if h.answer != nil {
			code = h.answer(r)
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(h.Server.Close)
	return h
}

func (h *fakeHarness) got() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.texts...)
}

// useHarness points the coordinator at h, as a restore does when the
// restored harness listens on a new port.
func (f *restartFixture) useHarness(t *testing.T, h *fakeHarness) {
	t.Helper()
	if err := execSQL(f.db, "UPDATE agent_status SET harness_port = ? WHERE session_name = ?",
		parsePort(t, h.URL), restartSession); err != nil {
		t.Fatalf("set harness port: %v", err)
	}
}

func (f *restartFixture) pendingNotices(t *testing.T) []int {
	t.Helper()
	notices, err := f.db.PendingMergeNotices(restartSession)
	if err != nil {
		t.Fatalf("PendingMergeNotices: %v", err)
	}
	prs := make([]int, 0, len(notices))
	for _, n := range notices {
		prs = append(prs, n.PR)
	}
	return prs
}

// nextWatcherTicks runs two ticks of the watcher that follows the stop.
// The row is no longer watching, so the watcher makes no gh call.
func (f *restartFixture) nextWatcherTicks(t *testing.T) {
	t.Helper()
	w := f.watcher(f.inst, newGHFake(t, prInfo{}))
	w.tick(context.Background())
	w.tick(context.Background())
}

func assertTexts(t *testing.T, got []string, want string) {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("notifications = %q, want one that contains %q", got, want)
	}
	if !strings.Contains(got[0], want) {
		t.Errorf("notification = %q, want it to contain %q", got[0], want)
	}
}

// endOutcome is a terminal outcome of the watcher. gh gives the gh answers
// that decide the outcome. When stopsInGH is set, the gh call that decides
// the outcome calls stop before it returns: the stop comes after the
// watcher read the outcome and before it writes the row.
type endOutcome struct {
	name      string
	status    string
	text      string
	stopsInGH bool
	gh        func(t *testing.T, stop func()) *ghFake
}

func viewThenStop(t *testing.T, stop func(), info prInfo) *ghFake {
	gh := newGHFake(t, info)
	gh.view = func(int) (prInfo, error) {
		stop()
		return info, nil
	}
	return gh
}

func endOutcomes(pr int) []endOutcome {
	mergedAt := "2026-10-21T09:00:00Z"
	return []endOutcome{
		{
			name: "merged by the watcher", status: "merged", stopsInGH: true,
			text: fmt.Sprintf("PR #%d merged. Please clean up the branch and worktree.", pr),
			gh: func(t *testing.T, stop func()) *ghFake {
				gh := newGHFake(t, prInfo{State: "OPEN", MergeStateStatus: "CLEAN"})
				gh.merge = func(int) ([]byte, error) {
					stop()
					return nil, nil
				}
				return gh
			},
		},
		{
			name: "merged out of band", status: "merged", stopsInGH: true,
			text: fmt.Sprintf("PR #%d merged out-of-band", pr),
			gh: func(t *testing.T, stop func()) *ghFake {
				return viewThenStop(t, stop, prInfo{State: "MERGED", MergedAt: &mergedAt})
			},
		},
		{
			name: "closed", status: "failed", stopsInGH: true,
			text: fmt.Sprintf("PR #%d closed without merge", pr),
			gh: func(t *testing.T, stop func()) *ghFake {
				return viewThenStop(t, stop, prInfo{State: "CLOSED"})
			},
		},
		{
			name: "conflict", status: "failed", stopsInGH: true,
			text: fmt.Sprintf("PR #%d has conflicts", pr),
			gh: func(t *testing.T, stop func()) *ghFake {
				return viewThenStop(t, stop, prInfo{State: "OPEN", MergeStateStatus: "DIRTY"})
			},
		},
		{
			name: "required check failed", status: "failed", stopsInGH: true,
			text: fmt.Sprintf("PR #%d CI failed: pr-gate", pr),
			gh: func(t *testing.T, stop func()) *ghFake {
				return viewThenStop(t, stop, blockedInfo("APPROVED", []checkEntry{
					{Name: "pr-gate", Status: "COMPLETED", Conclusion: "FAILURE"},
				}))
			},
		},
		{
			name: "update-branch refused", status: "failed", stopsInGH: true,
			text: "gh pr update-branch failed with an error that no retry fixes",
			gh: func(t *testing.T, stop func()) *ghFake {
				gh := newGHFake(t, prInfo{State: "OPEN", MergeStateStatus: "BEHIND", Mergeable: "MERGEABLE"})
				gh.update = func(int) error {
					stop()
					return errors.New(workflowScopeErr)
				}
				return gh
			},
		},
		{
			// tryMerge leaves the row watching when the stop comes before the
			// failure path (#3267), so the stop can come only in the request.
			name: "merge failed", status: "failed", stopsInGH: false,
			text: fmt.Sprintf("PR #%d merge failed: gh pr merge failed: GraphQL: not allowed", pr),
			gh: func(t *testing.T, _ func()) *ghFake {
				gh := newGHFake(t, prInfo{State: "OPEN", MergeStateStatus: "CLEAN"})
				gh.merge = func(int) ([]byte, error) {
					return []byte("GraphQL: not allowed"), errors.New("exit status 1")
				}
				return gh
			},
		},
	}
}

// TestStopAfterEnd_HarnessUp_CoordinatorGetsNotification is the defect of
// #3282: the stop cancels the context after the watcher read the outcome
// and before the notification. The coordinator gets the notification one
// time, and the next watcher does not send it again.
func TestStopAfterEnd_HarnessUp_CoordinatorGetsNotification(t *testing.T) {
	const pr = 201
	for _, oc := range endOutcomes(pr) {
		if !oc.stopsInGH {
			continue
		}
		t.Run(oc.name, func(t *testing.T) {
			f := newShutdownFixture(t, "inst-3282-up", pr)
			h := newFakeHarness(t, nil)
			f.useHarness(t, h)
			ctx, stop := context.WithCancel(context.Background())
			defer stop()

			f.watcher(f.inst, oc.gh(t, stop)).tick(ctx)
			if ctx.Err() == nil {
				t.Fatal("the gh answers did not stop the watcher")
			}
			f.assertRow(t, pr, oc.status)
			assertTexts(t, h.got(), oc.text)
			if got := f.pendingNotices(t); len(got) != 0 {
				t.Errorf("pending notices = %v, want none", got)
			}

			f.shutDown(t)
			f.nextWatcherTicks(t)
			if got := h.got(); len(got) != 1 {
				t.Errorf("notifications after the next watcher = %d, want 1", len(got))
			}
		})
	}
}

// TestStopAfterEnd_HarnessDown_NextWatcherSendsNotification covers a
// harness that the stop takes down: each request stops the watcher and
// fails. The row keeps the notification. After the restore, the next
// watcher sends it to the restored harness one time.
func TestStopAfterEnd_HarnessDown_NextWatcherSendsNotification(t *testing.T) {
	const pr = 202
	for _, oc := range endOutcomes(pr) {
		t.Run(oc.name, func(t *testing.T) {
			f := newShutdownFixture(t, "inst-3282-down", pr)
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			down := newFakeHarness(t, func(*http.Request) int {
				stop()
				return http.StatusServiceUnavailable
			})
			f.useHarness(t, down)

			f.watcher(f.inst, oc.gh(t, stop)).tick(ctx)
			f.assertRow(t, pr, oc.status)
			if got := down.got(); len(got) != 1 {
				t.Fatalf("requests to the harness that went down = %d, want 1", len(got))
			}
			if got := f.pendingNotices(t); !equalInts(got, []int{pr}) {
				t.Fatalf("pending notices after the stop = %v, want [%d]", got, pr)
			}

			f.shutDown(t)
			if got := f.abandonedByStop(t); len(got) != 0 {
				t.Errorf("rows of the stop = %v, want none: the row is ended", mergePRs(got))
			}
			restored := newFakeHarness(t, nil)
			f.useHarness(t, restored)
			f.nextWatcherTicks(t)
			assertTexts(t, restored.got(), oc.text)
			if got := f.pendingNotices(t); len(got) != 0 {
				t.Errorf("pending notices after the next watcher = %v, want none", got)
			}
		})
	}
}

// TestStopDuringNotification_AnswerInGrace_SentOnce covers a stop that
// comes while the request is in flight. The harness answers in the grace
// time, so the request completes, and the next watcher does not send the
// notification again.
func TestStopDuringNotification_AnswerInGrace_SentOnce(t *testing.T) {
	const pr = 203
	for _, oc := range endOutcomes(pr) {
		t.Run(oc.name, func(t *testing.T) {
			f := newShutdownFixture(t, "inst-3282-grace", pr)
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			h := newFakeHarness(t, func(*http.Request) int {
				stop()
				time.Sleep(50 * time.Millisecond)
				return http.StatusOK
			})
			f.useHarness(t, h)

			f.watcher(f.inst, oc.gh(t, func() {})).tick(ctx)
			f.assertRow(t, pr, oc.status)
			assertTexts(t, h.got(), oc.text)
			if got := f.pendingNotices(t); len(got) != 0 {
				t.Errorf("pending notices = %v, want none: the harness answered", got)
			}

			f.shutDown(t)
			f.nextWatcherTicks(t)
			if got := h.got(); len(got) != 1 {
				t.Errorf("notifications after the next watcher = %d, want 1", len(got))
			}
		})
	}
}

// TestStopDuringNotification_NoAnswerInGrace_NextWatcherSends covers a
// harness that does not answer. The request stops at the end of the grace
// time, the row keeps the notification, and the next watcher sends it.
func TestStopDuringNotification_NoAnswerInGrace_NextWatcherSends(t *testing.T) {
	const pr = 204
	oc := endOutcomes(pr)[0]
	f := newShutdownFixture(t, "inst-3282-hung", pr)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	hung := newFakeHarness(t, func(r *http.Request) int {
		stop()
		<-r.Context().Done()
		return http.StatusServiceUnavailable
	})
	f.useHarness(t, hung)

	start := time.Now()
	f.watcher(f.inst, oc.gh(t, func() {})).tick(ctx)
	if elapsed := time.Since(start); elapsed < notifyStopGrace || elapsed > StopWait {
		t.Errorf("tick took %v, want between notifyStopGrace (%v) and StopWait (%v)", elapsed, notifyStopGrace, StopWait)
	}
	f.assertRow(t, pr, oc.status)
	if got := f.pendingNotices(t); !equalInts(got, []int{pr}) {
		t.Fatalf("pending notices = %v, want [%d]", got, pr)
	}

	f.shutDown(t)
	restored := newFakeHarness(t, nil)
	f.useHarness(t, restored)
	f.nextWatcherTicks(t)
	assertTexts(t, restored.got(), oc.text)
}

// TestKeptNotice_TwoWatchers_SentOnce covers two live watchers of one
// session, as after a restore that cannot prove that the old sidecar
// stopped. Both read the kept notification before either sends it. Only
// one sends it.
func TestKeptNotice_TwoWatchers_SentOnce(t *testing.T) {
	const pr = 205
	f := newShutdownFixture(t, "inst-3282-two", pr)
	h := newFakeHarness(t, nil)
	f.useHarness(t, h)
	const text = "PR #205 merged. Please clean up the branch and worktree."
	if changed, err := f.db.TerminateMergeWithNotice(pr, "myrepo", "merged", "", text); err != nil || !changed {
		t.Fatalf("TerminateMergeWithNotice = %v, %v", changed, err)
	}
	notices, err := f.db.PendingMergeNotices(restartSession)
	if err != nil || len(notices) != 1 {
		t.Fatalf("PendingMergeNotices = %+v, %v, want one", notices, err)
	}

	a := f.watcher(f.inst, newGHFake(t, prInfo{}))
	b := f.watcher("inst-3282-other", newGHFake(t, prInfo{}))
	a.sendNotice(context.Background(), notices[0])
	b.sendNotice(context.Background(), notices[0])
	a.tick(context.Background())
	b.tick(context.Background())
	assertTexts(t, h.got(), text)
}

// TestStopAfterEnd_CancelledOrAbandoned_NoNotification covers a row that
// another path ends while gh pr merge runs, and a stop that cancels the
// context. The row keeps no notification, so neither the old watcher nor
// the next watcher sends one.
func TestStopAfterEnd_CancelledOrAbandoned_NoNotification(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
		end  func(f *restartFixture, t *testing.T, pr int)
	}{
		{"cancelled", "cancelled", func(f *restartFixture, t *testing.T, pr int) {
			if ok, err := f.db.CancelMerge(pr, "myrepo", f.inst); err != nil || !ok {
				t.Fatalf("CancelMerge = %v, %v", ok, err)
			}
		}},
		{"abandoned", "abandoned", func(f *restartFixture, t *testing.T, _ int) { f.shutDown(t) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const pr = 206
			f := newShutdownFixture(t, "inst-3282-ended", pr)
			h := newFakeHarness(t, nil)
			f.useHarness(t, h)
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			gh := newGHFake(t, prInfo{State: "OPEN", MergeStateStatus: "CLEAN"})
			gh.merge = func(int) ([]byte, error) {
				tc.end(f, t, pr)
				stop()
				return nil, nil
			}
			f.watcher(f.inst, gh).tick(ctx)

			f.assertRow(t, pr, tc.want)
			if got := f.pendingNotices(t); len(got) != 0 {
				t.Errorf("pending notices = %v, want none", got)
			}
			f.nextWatcherTicks(t)
			if got := h.got(); len(got) != 0 {
				t.Errorf("notifications = %q, want none", got)
			}
		})
	}
}

// piTestHarness is a harness name with the socket-pipe shape, as pi has.
// The suite does not import the pi adapter: the coordinator rows of the
// other tests carry the default harness name "pi", and they must take the
// HTTP path.
const piTestHarness = "pi-test-3282"

var registerPiTestHarness = sync.OnceFunc(func() {
	harness.MustRegister(harness.Registration{
		Name:    piTestHarness,
		Shape:   harness.TransportSocketPipe,
		Factory: func(string, *http.Client, string, string) harness.Harness { return nil },
	})
})

// piPrompt is one /prompt request to the host-API socket of a pi
// coordinator.
type piPrompt struct {
	Prompt    string `json:"prompt"`
	DeliverAs string `json:"deliver_as"`
}

// startPiHostAPI serves the host-API socket of sessionName and records each
// /prompt request.
func startPiHostAPI(t *testing.T, sessionName string) func() []piPrompt {
	t.Helper()
	sock, err := session.SidecarHostAPIPath(sessionName)
	if err != nil {
		t.Fatalf("SidecarHostAPIPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen %s: %v", sock, err)
	}
	var mu sync.Mutex
	var prompts []piPrompt
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p piPrompt
		_ = json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		prompts = append(prompts, p)
		mu.Unlock()
		_, _ = w.Write([]byte("{}"))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = os.Remove(sock)
	})
	return func() []piPrompt {
		mu.Lock()
		defer mu.Unlock()
		return append([]piPrompt(nil), prompts...)
	}
}

// TestPiCoordinator_GetsEachNotificationOnce checks that a pi coordinator
// gets the notification as before: one time, through the host-API socket,
// also when the stop cancels the context before the notification.
func TestPiCoordinator_GetsEachNotificationOnce(t *testing.T) {
	registerPiTestHarness()
	const (
		pr          = 207
		sessionName = "pi3282@main"
		inst        = "inst-3282-pi"
	)
	for _, stopped := range []bool{false, true} {
		t.Run(fmt.Sprintf("stop=%v", stopped), func(t *testing.T) {
			d := openTestDB(t)
			seedCoordinator(t, d, sessionName, inst, 0, "sid-pi")
			if err := execSQL(d, "UPDATE agent_status SET harness = ? WHERE session_name = ?", piTestHarness, sessionName); err != nil {
				t.Fatalf("set harness: %v", err)
			}
			prompts := startPiHostAPI(t, sessionName)
			if _, err := d.EnqueueMerge(pr, "myrepo", sessionName, inst, nil); err != nil {
				t.Fatalf("EnqueueMerge: %v", err)
			}
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			gh := newGHFake(t, prInfo{State: "OPEN", MergeStateStatus: "CLEAN"})
			gh.merge = func(int) ([]byte, error) {
				if stopped {
					stop()
				}
				return nil, nil
			}
			newTickWatcher(d, inst, sessionName, http.DefaultClient, gh).tick(ctx)

			got := prompts()
			if len(got) != 1 {
				t.Fatalf("prompts = %+v, want one", got)
			}
			if want := "PR #207 merged."; !strings.Contains(got[0].Prompt, want) || got[0].DeliverAs != "followUp" {
				t.Errorf("prompt = %+v, want text with %q, delivered as followUp", got[0], want)
			}
			if notices, err := d.PendingMergeNotices(sessionName); err != nil || len(notices) != 0 {
				t.Errorf("pending notices = %+v, %v, want none", notices, err)
			}

			next := newTickWatcher(d, inst, sessionName, http.DefaultClient, newGHFake(t, prInfo{}))
			next.tick(context.Background())
			if got := prompts(); len(got) != 1 {
				t.Errorf("prompts after the next watcher = %d, want 1", len(got))
			}
		})
	}
}
