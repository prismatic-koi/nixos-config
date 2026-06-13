package cmd

// prism escalate — hand a question to the coordinator in one step and pause
// the calling worker session in a new "escalated" state.
//
// Surface:
//
//	prism escalate [--to <session>] --prompt "<message>"
//	prism escalate [--to <session>] --prompt -          # read from stdin
//
// Without --to, the command auto-discovers the same-repo coordinator (the
// session whose root_agent_name = "coordinator", or the legacy <repo>@main
// row when the field is NULL). With multiple candidates, --to is required;
// with zero candidates, the worker still transitions to escalated and writes
// a self-marker but no prompt is delivered (a human is expected to pick up
// the worker via tmux).
//
// State machine:
//
//	active ──prism escalate──▶ escalated
//	escalated ──any turn_start──▶ active
//
// While the calling session is in `escalated`, the sidecar suppresses the
// `has finished` notification — the `session.escalated` bus event is the
// notification.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/promptdelivery"
	"github.com/prismatic-koi/prism/internal/session"
)

var escalateCmd = &cobra.Command{
	Use:   "escalate",
	Short: "Hand a question to the coordinator and enter the 'escalated' state",
	Long: `Send a prompt to the same-repo coordinator (auto-discovered or specified
via --to) and transition the calling session to the "escalated" state.

While escalated, the sidecar suppresses the "has finished" notification — the
session.escalated bus event is the notification, and the coordinator gets the
escalation prompt as a single targeted signal rather than two redundant ones.

Discovery rules:

  - exactly one same-repo coordinator candidate → auto-discover, send to it.
  - multiple same-repo coordinator candidates   → refuse without --to and
    print the candidate list.
  - no coordinator candidate found              → still transition to
    escalated; record a "no coordinator found, please wait for a human"
    marker in the calling session's own log. The worker stays paused.

Any incoming turn_start (from prism prompt, a human typing into tmux, or any
other source) clears the escalated state and resumes the worker normally.

Output:

  On success the command prints exactly one line to stdout, mirrored to
  stderr so bash-tool captures that combine the streams always surface the
  signal:

    prism escalate: OK delivered to <target> (delivery_id=<uuid>)

  The "OK" token is the first whitespace-delimited word after "escalate:"
  so callers can grep for it as the unambiguous success signal.

  With --json, stdout instead carries a single JSON object:

    {"delivered_to": "<target>", "delivery_id": "<uuid>", "replayed": false}

  In --json mode the human-readable line is NOT emitted on stdout (mutual
  exclusion). It may still be mirrored to stderr for log capture. On error,
  --json emits {"error": "<message>"} to stderr and exits non-zero.

Sender-side idempotency:

  Running prism escalate a second time within 5 minutes with the same
  (calling session, target, prompt text) triple as a previously-delivered
  escalation is a no-op: no new bus_messages row, no new agent_events rows,
  no re-delivery, no state-transition write. The replay invocation exits 0
  with stdout:

    prism escalate: OK already delivered to <target> (delivery_id=<prior>, age=<duration>)

  The success line is the verification of delivery — do NOT re-run to
  confirm the first call landed.`,
	Args: cobra.NoArgs,
	RunE: runEscalate,
}

func init() {
	addPromptFlags(escalateCmd)
	escalateCmd.Flags().String("to", "", `Explicit coordinator session to receive the escalation. Required when auto-discovery finds multiple coordinator candidates in the same repo. Optional otherwise.`)
	escalateCmd.Flags().Bool("json", false, `Emit a single JSON object to stdout instead of the human-readable success line. Shape: {"delivered_to":"<target>","delivery_id":"<uuid>","replayed":<bool>}. Errors emit {"error":"<msg>"} to stderr.`)
	escalateCmd.Flags().Duration("dedup-window", 5*time.Minute, "Override the sender-side dedup window for replay detection. Hidden — for tests and operator override only.")
	_ = escalateCmd.Flags().MarkHidden("dedup-window")
	rootCmd.AddCommand(escalateCmd)
}

// escalateDefaultDedupWindow is the default sender-side dedup window for
// `prism escalate`. A second invocation with byte-identical (from, target,
// prompt_text) within this window short-circuits as a replay. See #2018.
const escalateDefaultDedupWindow = 5 * time.Minute

func runEscalate(cmd *cobra.Command, args []string) error {
	promptText, err := requirePromptInput(cmd)
	if err != nil {
		return escalateReportError(cmd, err)
	}

	explicitTo, _ := cmd.Flags().GetString("to")
	jsonOut, _ := cmd.Flags().GetBool("json")
	dedupWindow, _ := cmd.Flags().GetDuration("dedup-window")
	if dedupWindow <= 0 {
		dedupWindow = escalateDefaultDedupWindow
	}
	opts := escalateOptions{jsonOut: jsonOut, dedupWindow: dedupWindow}

	// Container path: when running inside an isolated worker container, the
	// host-side prism CLI and DB are not directly accessible. Proxy to the
	// host sidecar's /escalate endpoint, which shells back to `prism escalate`
	// on the host with PRISM_HOST_API unset. The host-side invocation will
	// produce the success line; the proxy forwards its stdout/stderr to the
	// caller's streams so the OK signal reaches the container's bash tool.
	// (Without this re-emit, the container caller would see no output on
	// success — the exact symptom issue #2018 set out to eliminate.)
	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
		dedupArg := ""
		if cmd.Flags().Changed("dedup-window") {
			dedupArg = dedupWindow.String()
		}
		if err := proxyEscalate(apiURL, explicitTo, promptText, jsonOut, dedupArg); err != nil {
			return escalateReportError(cmd, err)
		}
		return nil
	}

	database, err := openDB()
	if err != nil {
		return escalateReportError(cmd, fmt.Errorf("open db: %w", err))
	}
	defer database.Close()

	// Derive the calling (source) session from CWD.
	cwd, _ := os.Getwd()
	if envSession := os.Getenv("PRISM_SESSION_NAME"); envSession != "" {
		// Tests and the host-API proxy may set PRISM_SESSION_NAME directly.
		if err := runEscalateForSessionOpts(database, envSession, explicitTo, promptText, opts); err != nil {
			return escalateReportError(cmd, err)
		}
		return nil
	}
	fromSession := deriveSessionNameFromCWD(cwd)
	if fromSession == "" {
		return escalateReportError(cmd, fmt.Errorf(
			"prism escalate: could not derive calling session from CWD %q\n"+
				"hint: run from inside a prism worktree, or set PRISM_SESSION_NAME",
			cwd,
		))
	}
	if err := runEscalateForSessionOpts(database, fromSession, explicitTo, promptText, opts); err != nil {
		return escalateReportError(cmd, err)
	}
	return nil
}

// escalateOptions carries the per-invocation switches that are not part of
// the core (database, fromSession, explicitTo, promptText) tuple.
type escalateOptions struct {
	jsonOut     bool
	dedupWindow time.Duration
}

// escalateReportError formats and emits an error for `prism escalate`.
// In --json mode it writes {"error":"<msg>"} to stderr; in human mode it
// returns the error so the cobra/main wrapper prints it. In both cases the
// returned error drives a non-zero exit code.
func escalateReportError(cmd *cobra.Command, err error) error {
	if err == nil {
		return nil
	}
	jsonOut, _ := cmd.Flags().GetBool("json")
	if !jsonOut {
		return err
	}
	payload := map[string]string{"error": err.Error()}
	if b, mErr := json.Marshal(payload); mErr == nil {
		fmt.Fprintln(os.Stderr, string(b))
	} else {
		fmt.Fprintf(os.Stderr, "{\"error\":%q}\n", err.Error())
	}
	// Returning a non-nil error still drives os.Exit(1) from main, but the
	// human formatter in main also prints err to stderr. Suppress that by
	// wrapping with a quiet-exit type so main only emits the JSON we wrote.
	return &quietExitErr{inner: err}
}

// quietExitErr suppresses main's default "fmt.Fprintln(os.Stderr, err)"
// rendering for --json error paths: main checks for the exitCoder interface
// and uses its ExitCode but only prints err.Error() when Error() is non-empty.
// We override Error() to return an empty string so nothing is written; the
// JSON error envelope has already been printed.
type quietExitErr struct{ inner error }

func (q *quietExitErr) Error() string { return "" }
func (q *quietExitErr) ExitCode() int { return 1 }
func (q *quietExitErr) Unwrap() error { return q.inner }

// runEscalateForSession is the testable core of runEscalate, parameterised on
// the calling session name so unit tests can drive it without the CWD walk.
// This is the public, backwards-compatible signature used by existing tests;
// new callers should use runEscalateForSessionOpts to thread --json and
// --dedup-window through.
func runEscalateForSession(database *db.DB, fromSession, explicitTo, promptText string) error {
	return runEscalateForSessionOpts(database, fromSession, explicitTo, promptText, escalateOptions{dedupWindow: escalateDefaultDedupWindow})
}

// runEscalateForSessionOpts is the full testable core, including the
// --json and --dedup-window switches that runEscalate threads from cobra.
func runEscalateForSessionOpts(database *db.DB, fromSession, explicitTo, promptText string, opts escalateOptions) error {
	if opts.dedupWindow <= 0 {
		opts.dedupWindow = escalateDefaultDedupWindow
	}
	selfStatus, err := database.CurrentStatus(fromSession)
	if err != nil {
		return fmt.Errorf("prism escalate: read self status: %w", err)
	}
	if selfStatus == nil {
		return fmt.Errorf("prism escalate: calling session %q has no agent_status row", fromSession)
	}
	repo := selfStatus.Repo
	if repo == "" {
		return fmt.Errorf("prism escalate: calling session %q has no repo recorded — escalate is only available from worktree-backed sessions", fromSession)
	}

	// Resolve the target.
	target, err := resolveEscalationTarget(database, repo, explicitTo)
	if err != nil {
		// Discovery error: do NOT transition state. Print and exit non-zero.
		return err
	}

	// Build payload metadata.
	branch := ""
	headSHA := ""
	if selfStatus.Worktree != "" {
		if ref, refErr := git.SymbolicRef(selfStatus.Worktree); refErr == nil {
			branch = strings.TrimPrefix(strings.TrimSpace(ref), "refs/heads/")
		}
		if sha, shaErr := git.ShortHash(selfStatus.Worktree); shaErr == nil {
			headSHA = strings.TrimSpace(sha)
		}
	}
	prNumbers := discoverPRNumbers(selfStatus.Worktree)
	verdicts := discoverLastReviewVerdicts(database, fromSession)

	targetName := ""
	var targetInstanceID *string
	if target != nil {
		targetName = target.SessionName
		targetInstanceID = target.InstanceID
	}

	payload := EscalationPayload{
		Source:     fromSession,
		Target:     targetName,
		Prompt:     promptText,
		PRNumbers:  prNumbers,
		Branch:     branch,
		HeadSHA:    headSHA,
		Verdicts:   verdicts,
		OccurredAt: time.Now().UTC().Format(time.RFC3339),
	}

	// Sender-side idempotency guard (issue #2018): if this is a byte-equal
	// re-run of a recent escalation we already delivered to the same target,
	// short-circuit before any state transition, agent_events write, or
	// bus_messages row. The session must currently be in `escalated` state
	// for the dedup path — if it has since transitioned out (e.g. an
	// incoming turn_start unstuck the worker), the second invocation is a
	// genuine re-escalation and proceeds normally.
	if target != nil && selfStatus.State == string(agent.StateEscalated) {
		prior, perr := database.FindRecentEquivalentBusMessage(fromSession, target.SessionName, promptText, opts.dedupWindow)
		if perr == nil && prior != nil {
			emitEscalateReplay(opts, target.SessionName, prior)
			return nil
		}
		if perr != nil {
			// Surface the lookup error to stderr but do not fail the call —
			// fall through to the normal path. A stale or unreadable bus
			// row should not prevent a genuine escalation.
			fmt.Fprintf(os.Stderr, "prism escalate: dedup lookup failed: %v (proceeding with fresh delivery)\n", perr)
		}
	}

	// Transition the calling session to escalated. This must happen even when
	// no coordinator was found (per the discovery rules), so do it before any
	// delivery attempt that might fail.
	if err := database.UpsertStatus(fromSession, selfStatus.Repo, selfStatus.Worktree, string(agent.StateEscalated), nil, nil); err != nil {
		return fmt.Errorf("prism escalate: transition to escalated: %w", err)
	}

	// Echo the escalation context into the calling session's own log so
	// reviewers and humans see it inline via `prism checkin <self>`.
	echoEscalationToSelf(database, fromSession, selfStatus, payload)

	// Write the session.escalated bus event for dashboard / downstream
	// consumers. This row sits alongside any later `session.finished` row,
	// distinct in name and payload.
	writeSessionEscalatedEvent(database, fromSession, selfStatus, payload)

	// Deliver to the coordinator. With zero candidates, target is nil and
	// we skip delivery entirely — the calling session is still escalated
	// and the bus event still fired (with empty target).
	if target == nil {
		fmt.Fprintf(os.Stderr,
			"prism escalate: no coordinator found in repo %q; session %q transitioned to escalated.\n"+
				"please wait for a human to come check on you.\n",
			repo, fromSession,
		)
		return nil
	}

	// Muted guard: when the calling session is muted, suppress the outbound
	// coordinator escalation notification. The state transition and the
	// session.escalated agent_events row are still written above so the
	// dashboard reflects reality; only the bus delivery is skipped. This
	// mirrors the notifyCoordinator suppression in internal/sidecar/notify.go
	// for the finish-notification path — outbound to the coordinator is the
	// suppression boundary, DB writes are unaffected. (#2013)
	if selfStatus.Muted {
		fmt.Fprintf(os.Stderr,
			"prism escalate: session %q is muted; coordinator notification suppressed (state still transitioned to escalated, bus event still written).\n",
			fromSession,
		)
		return nil
	}

	// Deliver via prism prompt machinery. We write the bus_messages row as
	// "delivered" only on success; the sidecar's prompt path already does this
	// for the audit trail, so we delegate by invoking the same code path that
	// `prism prompt` uses.
	deliveryID, err := deliverEscalationPrompt(database, fromSession, target, promptText)
	if err != nil {
		// Delivery failed but state is already escalated. Surface the error
		// so the worker knows; the bus event was already written so the
		// coordinator may still see the escalation through the bus.
		return fmt.Errorf("prism escalate: deliver prompt to %s: %w", target.SessionName, err)
	}

	emitEscalateSuccess(opts, target.SessionName, deliveryID)
	_ = targetInstanceID // surfaced via bus payload; not used directly here
	return nil
}

// emitEscalateSuccess writes the success signal for a fresh delivery.
// In human mode the same line goes to stdout AND stderr so a bash tool that
// captures either stream surfaces the OK signal. In --json mode stdout gets
// a single JSON object; the human line is mirrored to stderr only.
func emitEscalateSuccess(opts escalateOptions, target, deliveryID string) {
	human := fmt.Sprintf("prism escalate: OK delivered to %s (delivery_id=%s)", target, deliveryID)
	if opts.jsonOut {
		obj := map[string]any{
			"delivered_to": target,
			"delivery_id":  deliveryID,
			"replayed":     false,
		}
		if b, err := json.Marshal(obj); err == nil {
			fmt.Fprintln(os.Stdout, string(b))
		} else {
			fmt.Fprintf(os.Stdout, "{\"delivered_to\":%q,\"delivery_id\":%q,\"replayed\":false}\n", target, deliveryID)
		}
		fmt.Fprintln(os.Stderr, human)
		return
	}
	fmt.Fprintln(os.Stdout, human)
	fmt.Fprintln(os.Stderr, human)
}

// emitEscalateReplay writes the success signal for a deduped replay. The
// duration is formatted from time.Since(prior.SentAt) in the shortest
// human-readable unit (s/m/h).
func emitEscalateReplay(opts escalateOptions, target string, prior *db.BusMessage) {
	age := time.Since(prior.SentAt)
	ageStr := formatEscalateAge(age)
	human := fmt.Sprintf("prism escalate: OK already delivered to %s (delivery_id=%s, age=%s)", target, prior.ID, ageStr)
	if opts.jsonOut {
		ageSec := int64(age.Round(time.Second).Seconds())
		if ageSec < 0 {
			ageSec = 0
		}
		obj := map[string]any{
			"delivered_to": target,
			"delivery_id":  prior.ID,
			"replayed":     true,
			"age_seconds":  ageSec,
		}
		if b, err := json.Marshal(obj); err == nil {
			fmt.Fprintln(os.Stdout, string(b))
		} else {
			fmt.Fprintf(os.Stdout, "{\"delivered_to\":%q,\"delivery_id\":%q,\"replayed\":true,\"age_seconds\":%d}\n", target, prior.ID, ageSec)
		}
		fmt.Fprintln(os.Stderr, human)
		return
	}
	fmt.Fprintln(os.Stdout, human)
	fmt.Fprintln(os.Stderr, human)
}

// formatEscalateAge renders a duration as the shortest of "<N>s", "<N>m",
// or "<N>h". Sub-second durations clamp to "0s". This matches the format
// described in the acceptance criteria of issue #2018.
func formatEscalateAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int64(d.Round(time.Second).Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int64(d.Round(time.Minute).Minutes()))
	default:
		return fmt.Sprintf("%dh", int64(d.Round(time.Hour).Hours()))
	}
}

// resolveEscalationTarget applies the discovery rules from the issue:
//
//   - explicitTo set: look up that session by name; error if missing.
//   - explicitTo unset and len(candidates) == 1: return that candidate.
//   - explicitTo unset and len(candidates) > 1: error listing candidates.
//   - explicitTo unset and len(candidates) == 0: return (nil, nil) so the
//     caller can transition state without delivery.
func resolveEscalationTarget(database *db.DB, repo, explicitTo string) (*db.Status, error) {
	if explicitTo != "" {
		st, err := database.CurrentStatus(explicitTo)
		if err != nil {
			return nil, fmt.Errorf("prism escalate: look up --to %q: %w", explicitTo, err)
		}
		if st == nil {
			return nil, fmt.Errorf("prism escalate: --to session %q not found in agent_status\nrun `prism sessions list` to see available sessions", explicitTo)
		}
		if st.EndedAt != nil {
			return nil, fmt.Errorf("prism escalate: --to session %q has ended", explicitTo)
		}
		return st, nil
	}

	candidates, err := database.CoordinatorCandidatesForRepo(repo)
	if err != nil {
		return nil, fmt.Errorf("prism escalate: discover coordinator candidates for repo %q: %w", repo, err)
	}
	switch len(candidates) {
	case 0:
		return nil, nil
	case 1:
		c := candidates[0]
		return &c, nil
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "prism escalate: multiple coordinator candidates in repo %q; pass --to to choose one:\n", repo)
		for _, c := range candidates {
			fmt.Fprintf(&b, "  - %s\n", c.SessionName)
		}
		return nil, fmt.Errorf("%s", b.String())
	}
}

// EscalationPayload is the JSON payload carried by both the local-log
// `escalation` event and the bus `session.escalated` event.
type EscalationPayload struct {
	Source     string   `json:"source"`               // calling worker session
	Target     string   `json:"target,omitempty"`     // coordinator session (empty when none)
	Prompt     string   `json:"prompt"`               // user-supplied message body
	PRNumbers  []string `json:"pr_numbers,omitempty"` // discoverable open PRs
	Branch     string   `json:"branch,omitempty"`     // worker branch
	HeadSHA    string   `json:"head_sha,omitempty"`   // short HEAD SHA
	Verdicts   []string `json:"verdicts,omitempty"`   // last review-cycle verdicts
	OccurredAt string   `json:"occurred_at"`          // RFC3339
}

// echoEscalationToSelf writes a self-marker event of type "escalation" into
// the calling session's own event log so `prism checkin <self>` shows the
// escalation context inline. This is in addition to the bus event and is
// distinct from delivery to the coordinator.
func echoEscalationToSelf(database *db.DB, fromSession string, selfStatus *db.Status, payload EscalationPayload) {
	body, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "prism escalate: marshal self echo payload: %v\n", err)
		return
	}
	ev := db.Event{
		ID:          uuid.New().String(),
		SessionName: fromSession,
		Repo:        selfStatus.Repo,
		Worktree:    selfStatus.Worktree,
		InstanceID:  selfStatus.InstanceID,
		Type:        "escalation",
		Payload:     string(body),
		CreatedAt:   time.Now(),
	}
	if err := database.WriteEvent(ev); err != nil {
		fmt.Fprintf(os.Stderr, "prism escalate: write self echo event: %v\n", err)
	}
}

// writeSessionEscalatedEvent writes a bus-shaped event of type
// "session.escalated" into agent_events for the calling session. This is the
// new event type distinct from "session.finished": handlers that subscribe
// only to session.finished receive nothing for an escalation.
func writeSessionEscalatedEvent(database *db.DB, fromSession string, selfStatus *db.Status, payload EscalationPayload) {
	body, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "prism escalate: marshal bus event payload: %v\n", err)
		return
	}
	ev := db.Event{
		ID:          uuid.New().String(),
		SessionName: fromSession,
		Repo:        selfStatus.Repo,
		Worktree:    selfStatus.Worktree,
		InstanceID:  selfStatus.InstanceID,
		Type:        "session.escalated",
		Payload:     string(body),
		CreatedAt:   time.Now(),
	}
	if err := database.WriteEvent(ev); err != nil {
		fmt.Fprintf(os.Stderr, "prism escalate: write session.escalated event: %v\n", err)
	}
}

// deliverEscalationPrompt routes the prompt to the target coordinator. It
// reuses the same delivery machinery as `prism prompt`: HTTP for HTTP harnesses
// sessions, host-API socket for socket-pipe (PI) sessions.
//
// The audit row in bus_messages is written with delivered_at set on success.
// from_session is the calling worker session. The returned string is the
// bus_messages.id (a UUID) that the caller surfaces as the delivery_id in
// the success line.
func deliverEscalationPrompt(database *db.DB, fromSession string, target *db.Status, promptText string) (string, error) {
	msg := db.BusMessage{
		ID:           uuid.New().String(),
		FromSession:  fromSession,
		ToSession:    target.SessionName,
		ToInstanceID: target.InstanceID,
		Repo:         target.Repo,
		Text:         promptText,
		Urgency:      "normal",
		SentAt:       time.Now(),
	}

	// Reuse the existing delivery primitives. The "deliver_as=followUp" mode
	// queues the message until the coordinator's current turn ends, matching
	// the semantics of finish notifications today (the coordinator should not
	// be steered mid-stream by a worker escalation).
	if target.Harness != nil && *target.Harness != "" {
		// Try the harness-aware path. We mirror cmd/prompt.go's runPrompt
		// inline rather than calling it because runPrompt resolves the
		// from_session via CWD walk, which is brittle here (we already know
		// it). The actual transport selection is what we want to share.
		if err := deliverEscalationToTarget(target, promptText); err != nil {
			if writeErr := database.WriteBusMessageFailed(msg); writeErr != nil {
				fmt.Fprintf(os.Stderr, "prism escalate: write failed audit: %v\n", writeErr)
			}
			return "", err
		}
	} else {
		// Pre-migration row with no harness column. Fall back to HTTP delivery.
		if err := deliverEscalationToTarget(target, promptText); err != nil {
			if writeErr := database.WriteBusMessageFailed(msg); writeErr != nil {
				fmt.Fprintf(os.Stderr, "prism escalate: write failed audit: %v\n", writeErr)
			}
			return "", err
		}
	}

	if err := database.WriteBusMessageDelivered(msg); err != nil {
		fmt.Fprintf(os.Stderr, "prism escalate: write delivered audit: %v\n", err)
	}
	return msg.ID, nil
}

// deliverEscalationToTarget delivers the escalation to the target session.
// Uses promptdelivery which dispatches based on harness transport shape.
func deliverEscalationToTarget(target *db.Status, promptText string) error {
	return promptdelivery.DeliverToSession(target.SessionName, target, promptText, nil, "", "followUp")
}

// discoverPRNumbers best-effort returns open PRs whose head matches the
// worker's branch via `gh pr list`. Returns nil silently on any error so the
// escalation never fails on metadata gathering.
func discoverPRNumbers(worktree string) []string {
	if worktree == "" {
		return nil
	}
	branch, err := git.SymbolicRef(worktree)
	if err != nil {
		return nil
	}
	branch = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(branch), "refs/heads/"))
	if branch == "" {
		return nil
	}
	// Use gh pr list scoped to this branch. Best-effort; silent on any error.
	out, err := runCmdInDir(worktree, "gh", "pr", "list", "--head", branch, "--state", "open", "--json", "number", "--jq", ".[].number")
	if err != nil {
		return nil
	}
	var prs []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			prs = append(prs, line)
		}
	}
	return prs
}

// discoverLastReviewVerdicts scans the calling session's recent
// `review_complete` audit events (best-effort) for verdict strings. Returns
// nil when no verdicts are recorded, which is the common case for a worker
// that has not run `prism review`.
func discoverLastReviewVerdicts(database *db.DB, fromSession string) []string {
	// The dashboard / monitor records review verdicts on the parent worker via
	// the audit-event path. We scan the most recent ~20 audit events for any
	// verdict-bearing rows. This is intentionally tolerant: a missing or
	// malformed payload is silently dropped.
	events, err := database.QueryAuditEvents(fromSession, 0, "", 20)
	if err != nil {
		return nil
	}
	var out []string
	for _, ev := range events {
		// Look for verdict markers in payloads. We check both "verdict" and
		// "result" fields to tolerate shape evolution.
		var p map[string]any
		if err := json.Unmarshal([]byte(ev.Payload), &p); err != nil {
			continue
		}
		if v, ok := p["verdict"].(string); ok && v != "" {
			out = append(out, v)
			continue
		}
		if v, ok := p["result"].(string); ok && v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// proxyEscalate forwards the escalate request to the host sidecar. The
// sidecar handler shells out to `prism escalate` on the host, where the
// direct DB path runs. The host-side child's stdout and stderr are returned
// in the response body and re-emitted on the local streams here so the
// container caller sees the success/replay line (and — in --json mode — the
// JSON envelope on stdout). Without this round-trip, the container path
// would be silent on success, reproducing the symptom issue #2018 set out
// to eliminate.
func proxyEscalate(apiURL, explicitTo, promptText string, jsonOut bool, dedupWindow string) error {
	return proxyEscalateWithWriters(apiURL, explicitTo, promptText, jsonOut, dedupWindow, os.Stdout, os.Stderr)
}

// escalateProxyResponse is the host-API /escalate response shape. stdout and
// stderr are the captured byte streams of the host-side `prism escalate`
// subprocess; the container-side caller writes them verbatim to its own
// stdout/stderr so the byte content is identical to a host invocation
// (modulo trailing whitespace). On failure error is non-empty; the streams
// are still populated so partial-success progress lines survive.
type escalateProxyResponse struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Error  string `json:"error,omitempty"`
}

// proxyEscalateWithWriters is the testable form of proxyEscalate: stdout and
// stderr destinations are injectable so unit tests can capture forwarded
// output without redirecting os.Stdout / os.Stderr.
//
// We do NOT route through proxyToHostAPI here because that helper short-
// circuits the body on any HTTP >= 400, swallowing the stdout/stderr fields
// the handler attaches to error responses. We need both streams forwarded
// unconditionally so the caller sees partial output and the underlying
// cause even when the host child failed. This is parity with
// proxyCleanupToHostAPIWithWriters (issue #1527).
func proxyEscalateWithWriters(apiURL, explicitTo, promptText string, jsonOut bool, dedupWindow string, stdout, stderr io.Writer) error {
	body := map[string]any{
		"prompt": promptText,
	}
	if explicitTo != "" {
		body["to"] = explicitTo
	}
	if jsonOut {
		body["json"] = true
	}
	if strings.TrimSpace(dedupWindow) != "" {
		body["dedup_window"] = dedupWindow
	}
	// Pass our session name so the host can identify the calling session
	// without the CWD walk (the host-side process spawned by the sidecar
	// inherits the host CWD, not the container's worker CWD).
	if envSession := os.Getenv("PRISM_SESSION_NAME"); envSession != "" {
		body["from"] = envSession
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("proxyEscalate: marshal request body: %w", err)
	}

	client, reqURL, err := newEscalateHostAPIClient(apiURL)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, reqURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("proxyEscalate: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return fmt.Errorf("host-API /escalate: read response: %w", readErr)
	}

	var parsed escalateProxyResponse
	if len(respBody) > 0 {
		if unmarshalErr := json.Unmarshal(respBody, &parsed); unmarshalErr != nil {
			return fmt.Errorf("host-API /escalate: unmarshal response: %w (body=%s)", unmarshalErr, strings.TrimSpace(string(respBody)))
		}
	}
	// Forward the host-side streams unconditionally — even on error.
	if parsed.Stdout != "" {
		_, _ = io.WriteString(stdout, parsed.Stdout)
	}
	if parsed.Stderr != "" {
		_, _ = io.WriteString(stderr, parsed.Stderr)
	}
	if resp.StatusCode >= 400 {
		if parsed.Error != "" {
			return fmt.Errorf("host-API /escalate: %s", parsed.Error)
		}
		return fmt.Errorf("host-API /escalate: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// newEscalateHostAPIClient resolves an http.Client and request URL for the
// /escalate endpoint, supporting both the unix:// and http:// PRISM_HOST_API
// shapes used elsewhere in this package (parity with proxyToHostAPI).
func newEscalateHostAPIClient(apiURL string) (*http.Client, string, error) {
	if strings.HasPrefix(apiURL, "http://") {
		return newTCPHostAPIClient(), apiURL + "/escalate", nil
	}
	sockPath, parseErr := parseUnixSocketURL(apiURL)
	if parseErr != nil {
		return nil, "", fmt.Errorf("PRISM_HOST_API %q: %w", apiURL, parseErr)
	}
	return newHostAPIClient(sockPath), "http://prism-hostapi/escalate", nil
}

// runCmdInDir runs name with args in dir and returns stdout. A non-zero exit
// returns an error that includes the stderr output.
func runCmdInDir(dir, name string, args ...string) (string, error) {
	c := exec.Command(name, args...)
	c.Dir = dir
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// session.NameFor lookup helper passthrough — keeps the import live for
// the test path (testers stub PRISM_SESSION_NAME).
var _ = session.NameFor
