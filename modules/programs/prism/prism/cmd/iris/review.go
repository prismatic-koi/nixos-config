package main

// review.go — `iris review <pr>` subcommand.
//
// `iris review <pr-number>` is the iris-native analogue of `prism review
// <pr>`: an async 5-agent review orchestrator that spawns the canonical
// review agents (review-goal, review-code, review-security, review-qa,
// review-context) under a shared group_id, registers them in
// session_groups, and returns immediately with an acknowledgement.
//
// All heavy lifting lives in the daemon — this CLI is pure plumbing:
//
//   1. Resolve the calling session (IRIS_SESSION_NAME / PRISM_SESSION_NAME).
//   2. Validate flags (--only against the canonical agent set, --timeout
//      as a Go duration, --rebase as a literal bool).
//   3. Verify the PR exists via `gh pr view <n>` BEFORE spawning anything.
//   4. If --rebase: run `git fetch origin && git rebase origin/main &&
//      git push --force-with-lease` in the calling worktree. Failure of
//      any step exits non-zero before the daemon is contacted.
//   5. Dial the iris daemon client socket (canonical "daemon not running"
//      error when unreachable).
//   6. Send a review_spawn frame.
//   7. Read the review_spawned ack (or error frame) and print a compact
//      acknowledgement matching prism's "Review in progress" line.
//
// Async semantics: the actual review cycle runs in the daemon. The calling
// session receives the review-complete prompt via prompt_deliver once all
// agents reach a terminal state (sync.Once-guarded; exactly-once).

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/review"
)

// reviewDialTimeout bounds the time we spend dialling the daemon socket.
// Matches the spawn-side budget — the daemon is local, so anything longer
// than a couple of seconds is almost certainly "daemon not running".
const reviewDialTimeout = 2 * time.Second

// reviewAckTimeout bounds how long we wait for a review_spawned ack after
// the request frame is sent. The daemon's spawn work for 5 review agents
// completes well inside a few seconds in normal operation; the generous
// budget surfaces hangs as a clear error rather than blocking forever.
const reviewAckTimeout = 60 * time.Second

// defaultReviewTimeout is the per-agent review timeout sent to the daemon
// when --timeout is not specified. Matches prism's default.
const defaultReviewTimeout = 10 * time.Minute

// reviewCmd is the `iris review <pr>` cobra subcommand.
var reviewCmd = &cobra.Command{
	Use:   "review <pr-number>",
	Short: "Spawn 5 review agents for a PR and return immediately (async)",
	Long: `iris review spawns 5 review-agent sessions under the calling iris session,
registers them as a single group in session_groups, and returns immediately
with a review-in-progress acknowledgement.

The 5 standard review agents are:

  - review-goal      — does this PR actually fix what the issue asked for?
  - review-code      — code quality, correctness, idiomatic style
  - review-security  — secrets, permissions, attack surface
  - review-qa        — test coverage, edge cases, regressions
  - review-context   — design/architecture fit, blast radius

Each agent gets its own iris session named <parent>~review-N-<agent>, where
N is incremented on each invocation. Sessions are visible via
'iris sessions list' and persist until 'iris cleanup' is invoked.

A background watcher on the daemon polls for completion and delivers a
single review-complete prompt to the calling session via prompt_deliver
once every member reaches a terminal state ('finished' or 'error'). The
delivery is exactly-once: a defensive double-fire is deduplicated by the
watcher's sync.Once guard.

Do NOT commit, merge, or announce completion until the review-complete
prompt arrives.

# Pre-flight ancestor gate (#1518 parity)

Before spawning any agents, 'iris review' runs a one-shot strict-ancestor
check: 'git merge-base --is-ancestor origin/main HEAD'. If origin/main is
not an ancestor of HEAD the command refuses, exits non-zero, and prints
the number of commits behind plus the recommended fix. No agents spawn
and the round counter is unaffected on a gate refusal.

  --rebase performs fetch + rebase + force-push inline as the opt-in fix.
  On rebase conflict the rebase is aborted, HEAD is restored, and the
  command exits non-zero — the worktree is never left mid-rebase.

# Flags

  --timeout <dur>    per-agent timeout (default 10m)
  --only <csv>       run only the named agents (e.g. review-goal,review-code)
  --rebase           fetch origin/main, rebase HEAD onto it, force-push inline
                     before running the review (opt-in fix for the gate)
  --socket <path>    iris daemon socket (default ~/.local/state/iris/iris.sock)

# Exit codes

  0  — review spawn acknowledged; the watcher is running.
  1  — usage error, daemon down, PR not found, in-progress group, etc.`,
	Args:          cobra.ExactArgs(1),
	RunE:          runReviewCmd,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	reviewCmd.Flags().Duration("timeout", defaultReviewTimeout, "Per-agent timeout (default 10m)")
	reviewCmd.Flags().String("only", "", "Comma-separated subset of agents to run (e.g. review-goal,review-code). Empty = all 5.")
	reviewCmd.Flags().Bool("rebase", false, "Fetch origin/main, rebase HEAD onto it, and force-push before running the review")
	reviewCmd.Flags().String("socket", "", "Path to the iris daemon client socket (default: ~/.local/state/iris/iris.sock)")
	rootCmd.AddCommand(reviewCmd)
}

// runReviewCmd is the cobra entry point. Splits validation from wire I/O so
// the integration test can hit runReviewAt directly with a fake daemon
// socket and a fake gh probe.
func runReviewCmd(cmd *cobra.Command, args []string) error {
	prNumber := args[0]
	timeoutFlag, _ := cmd.Flags().GetDuration("timeout")
	onlyFlag, _ := cmd.Flags().GetString("only")
	rebaseFlag, _ := cmd.Flags().GetBool("rebase")
	sockPath := resolveSocketPath(cmd)

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("iris review: resolve cwd: %w", err)
	}

	opts := reviewRunOpts{
		PRNumber:        prNumber,
		Timeout:         timeoutFlag,
		Only:            onlyFlag,
		Rebase:          rebaseFlag,
		SockPath:        sockPath,
		Parent:          lookupIrisParentSession(),
		Worktree:        cwd,
		PRVerifier:      ghPRVerifier,
		PreflightRunner: defaultPreflightRunner,
		Out:             os.Stdout,
	}
	return runReviewAt(cmd.Context(), opts)
}

// reviewRunOpts bundles the validated CLI inputs and the swappable
// host-side effects (PR verifier, preflight runner) so the wire layer can
// be driven from a test without forking gh / git.
type reviewRunOpts struct {
	PRNumber  string
	Timeout   time.Duration
	Only      string
	Rebase    bool
	SockPath  string
	Parent    string
	// Worktree is the absolute path of the calling worktree, used by the
	// preflight ancestor gate. Defaults to the CLI's cwd.
	Worktree   string
	PRVerifier func(prNumber string) error
	// PreflightRunner implements the pre-flight ancestor / rebase gate.
	// Injectable so tests can drive runReviewAt without a real git repo.
	PreflightRunner func(opts preflightInput) error
	Out             io.Writer
}

// preflightInput is the test-seam input for the preflight runner. It
// mirrors the subset of review.PreflightOpts iris callers need.
type preflightInput struct {
	Worktree string
	Rebase   bool
	OnProgress func(line string)
}

// runReviewAt is the testable core. It performs:
//   1. Local validation (parent resolution, --only parsing).
//   2. PR existence check via the injected verifier.
//   3. Pre-flight ancestor gate: refuse if behind origin/main, with
//      --rebase the opt-in inline fix (#1518 parity).
//   4. Dial + send review_spawn + read review_spawned (or error).
func runReviewAt(ctx context.Context, opts reviewRunOpts) error {
	if opts.PRNumber == "" {
		return errors.New("iris review: <pr-number> is required")
	}
	if opts.Parent == "" {
		return errors.New(
			"iris review: could not determine calling session\n" +
				"hint: run from inside an iris-managed session, or set IRIS_SESSION_NAME",
		)
	}

	// --only validation happens up-front so an invalid name exits before
	// any spawn-side effects (matching the AC).
	requested, err := parseOnlyFlag(opts.Only)
	if err != nil {
		return err
	}

	// PR existence check BEFORE any spawn (AC: "exits non-zero before
	// spawning anything").
	if opts.PRVerifier == nil {
		opts.PRVerifier = ghPRVerifier
	}
	if err := opts.PRVerifier(opts.PRNumber); err != nil {
		return err
	}

	// Pre-flight ancestor / rebase gate (#1518 parity).
	//
	// Refuse if origin/main is not an ancestor of HEAD; --rebase performs
	// fetch + rebase + force-push inline as the opt-in fix. Runs BEFORE
	// the daemon is contacted so a gate failure does not register a
	// review group or increment the round counter (the round number is
	// derived from per-agent session rows, which only get written by the
	// daemon after a successful review_spawn).
	//
	// On rebase conflict the gate aborts the rebase and restores HEAD
	// before returning a non-zero error — never leaves the worktree
	// mid-rebase. See internal/review/preflight.go for the contract.
	if opts.PreflightRunner == nil {
		opts.PreflightRunner = defaultPreflightRunner
	}
	if opts.Worktree == "" {
		return errors.New("iris review: preflight: worktree path is required (set cwd to the calling worktree)")
	}
	if err := opts.PreflightRunner(preflightInput{
		Worktree:   opts.Worktree,
		Rebase:     opts.Rebase,
		OnProgress: func(line string) { fmt.Fprintln(opts.Out, line) },
	}); err != nil {
		return err
	}

	// Mint a delivery_id per run (#1695 contract: exactly-once delivery).
	deliveryID := uuid.NewString()

	// Send the review_spawn frame and read the ack.
	ack, err := sendReviewSpawn(ctx, opts.SockPath, iris.ClientReviewSpawnFrame{
		Type:       iris.ClientFrameReviewSpawn,
		Parent:     opts.Parent,
		PRNumber:   opts.PRNumber,
		AgentNames: requested,
		Timeout:    opts.Timeout.String(),
		DeliveryID: deliveryID,
	})
	if err != nil {
		return err
	}

	printReviewAck(opts.Out, ack, opts.PRNumber)
	return nil
}

// parseOnlyFlag splits the --only CSV, validates each token against the
// canonical agent list, and returns the resolved subset (empty when the
// flag was empty — meaning "all").
func parseOnlyFlag(csv string) ([]string, error) {
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return nil, nil
	}
	known := make(map[string]bool, len(iris.ReviewAgentNames))
	for _, a := range iris.ReviewAgentNames {
		known[a] = true
	}
	var out []string
	var unknown []string
	seen := make(map[string]bool)
	for _, raw := range strings.Split(csv, ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if !known[name] {
			unknown = append(unknown, name)
			continue
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf(
			"iris review: --only contains unknown agent name(s): %s — valid names: %s",
			strings.Join(unknown, ", "), strings.Join(iris.ReviewAgentNames, ", "),
		)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf(
			"iris review: --only must name at least one of: %s",
			strings.Join(iris.ReviewAgentNames, ", "),
		)
	}
	return out, nil
}

// lookupIrisParentSession resolves the calling iris session name from the
// environment. Iris does not depend on tmux, so no tmux fallback is
// attempted (issue #1766). Resolution order:
//
//   1. IRIS_SESSION_NAME — set by Supervisor.buildEnv when iris launches
//      its pi child (the canonical signal).
//   2. PRISM_SESSION_NAME — fall-back for callers that ran under the prism
//      runtime (rare; supports the iris-spawned-from-prism migration).
//
// Returns "" when no session can be identified. Callers should surface a
// clear error pointing at --session and the env-var contract.
func lookupIrisParentSession() string {
	if s := os.Getenv("IRIS_SESSION_NAME"); s != "" {
		return s
	}
	if s := os.Getenv("PRISM_SESSION_NAME"); s != "" {
		return s
	}
	return ""
}

// ghPRVerifier verifies that the named PR exists by invoking `gh pr view
// <n>`. Returns nil on exit-0; a clear, user-facing error otherwise.
func ghPRVerifier(prNumber string) error {
	cmd := exec.Command("gh", "pr", "view", prNumber)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	return fmt.Errorf(
		"iris review: PR #%s not found or gh failed: %v\n%s\n"+
			"hint: verify with `gh pr view %s` and ensure gh is authenticated",
		prNumber, err, strings.TrimSpace(string(out)), prNumber,
	)
}

// defaultPreflightRunner delegates to the shared review.Preflight helper
// (issue #1518). It performs the strict-ancestor check against origin/main
// and, when in.Rebase is true, runs fetch + rebase + force-push inline.
//
// Refusal (branch behind main) is surfaced as a *review.PreflightError
// with .Refused=true; we re-wrap the message under the "iris review:"
// prefix so the user-facing wording matches prism review's gate but
// names the iris CLI as the source.
//
// We intentionally reuse the prism review helper rather than
// reimplementing the gate: the strict-ancestor check, fetch-failure
// handling, rebase abort + HEAD restore, and force-with-lease push are
// all subtle to get right (PR #1518 establishes the canonical contract).
// Duplicating them in iris would invite drift. The helper does not touch
// the iris DB or any prism-internal state; it is purely a git wrapper.
func defaultPreflightRunner(in preflightInput) error {
	err := review.Preflight(review.PreflightOpts{
		Worktree:   in.Worktree,
		Rebase:     in.Rebase,
		OnProgress: in.OnProgress,
	})
	if err == nil {
		return nil
	}
	// The prism helper prefixes its messages with "prism review:". Replace
	// the leading prefix with "iris review:" so users see the CLI they
	// invoked named in the error. The body wording (rebase hints, etc.) is
	// otherwise identical and matches the documented prism behaviour.
	var pe *review.PreflightError
	if errors.As(err, &pe) {
		msg := strings.TrimPrefix(pe.Msg, "prism review:")
		if msg == pe.Msg {
			msg = pe.Msg
		}
		return fmt.Errorf("iris review:%s", msg)
	}
	return fmt.Errorf("iris review: preflight: %w", err)
}

// sendReviewSpawn dials the daemon, writes the review_spawn frame, and
// reads frames until either a review_spawned ack or an error frame
// arrives (or the read deadline expires).
//
// Read-deadline contract: we set ONE read deadline at reviewAckTimeout
// after sending the request frame and do NOT refresh it across iterations
// of the read loop. The daemon's handleReviewSpawn writes exactly one
// response frame per request (review_spawned on success, error otherwise);
// the loop only iterates when the daemon sends an unknown / malformed /
// non-matching frame, which is not part of today's wire contract. If a
// future protocol revision adds pre-ack progress frames here, refresh the
// deadline inside the loop or switch to a per-iteration budget — until
// then the one-shot deadline is intentional and bounds total wait
// regardless of how many spurious frames a misbehaving daemon emits.
func sendReviewSpawn(ctx context.Context, sockPath string, frame iris.ClientReviewSpawnFrame) (*iris.DaemonReviewSpawnedFrame, error) {
	// Daemon-down detection: stat first so we can give the canonical
	// "systemctl --user start iris" error rather than a raw dial error.
	if _, err := os.Stat(sockPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf(
				"iris daemon not running: socket %s does not exist; start it with `systemctl --user start iris`",
				sockPath,
			)
		}
		return nil, fmt.Errorf("iris review: stat socket %s: %w", sockPath, err)
	}
	d := net.Dialer{Timeout: reviewDialTimeout}
	conn, err := d.DialContext(ctx, "unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf(
			"iris daemon not running: cannot connect to %s (%v); start it with `systemctl --user start iris`",
			sockPath, err,
		)
	}
	defer conn.Close()

	data, err := json.Marshal(frame)
	if err != nil {
		return nil, fmt.Errorf("iris review: marshal review_spawn: %w", err)
	}
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		return nil, fmt.Errorf("iris review: send review_spawn: %w", err)
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetReadDeadline(time.Now())
		case <-done:
		}
	}()

	if err := conn.SetReadDeadline(time.Now().Add(reviewAckTimeout)); err != nil {
		return nil, fmt.Errorf("iris review: set read deadline: %w", err)
	}

	r := bufio.NewReaderSize(conn, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, errors.New("iris review: daemon closed connection before sending review_spawned ack")
			}
			var nerr net.Error
			if errors.As(err, &nerr) && nerr.Timeout() {
				return nil, fmt.Errorf("iris review: timed out after %s waiting for review_spawned ack", reviewAckTimeout)
			}
			return nil, fmt.Errorf("iris review: read ack: %w", err)
		}
		var generic struct {
			Type        string `json:"type"`
			RequestType string `json:"request_type"`
			Message     string `json:"message"`
		}
		if err := json.Unmarshal(line, &generic); err != nil {
			// Forward-compat: skip malformed frames, keep reading.
			fmt.Fprintf(os.Stderr, "[iris review] warning: ignoring malformed frame: %v\n", err)
			continue
		}
		switch generic.Type {
		case iris.DaemonFrameReviewSpawned:
			var ack iris.DaemonReviewSpawnedFrame
			if err := json.Unmarshal(line, &ack); err != nil {
				return nil, fmt.Errorf("iris review: decode review_spawned: %w", err)
			}
			return &ack, nil
		case iris.DaemonFrameError:
			// Surface the daemon-side error verbatim.
			if generic.RequestType == "" || generic.RequestType == iris.ClientFrameReviewSpawn {
				return nil, fmt.Errorf("iris review: %s", generic.Message)
			}
			// Unrelated error frame — log and keep reading.
			fmt.Fprintf(os.Stderr, "[iris review] note: unrelated error frame (request_type=%q): %s\n", generic.RequestType, generic.Message)
		default:
			// Unknown frame type — log and keep reading (forward-compat).
			fmt.Fprintf(os.Stderr, "[iris review] note: skipping pre-ack frame of type %q\n", generic.Type)
		}
	}
}

// printReviewAck writes a compact acknowledgement to out, mirroring
// prism's "Review in progress" line so output looks familiar to users
// switching between the two CLIs.
func printReviewAck(out io.Writer, ack *iris.DaemonReviewSpawnedFrame, prNumber string) {
	fmt.Fprintf(out, "Review in progress — PR #%s, round %d (group: %s)\n", prNumber, ack.Round, ack.GroupID)
	fmt.Fprintf(out, "Parent session: %s\n", ack.Parent)
	fmt.Fprintln(out, "Spawned review agents:")
	for _, m := range ack.Members {
		fmt.Fprintf(out, "  - %-16s → %s\n", m.Agent, m.SessionName)
	}
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Wait for the review-complete prompt before committing further changes,")
	fmt.Fprintln(out, "merging, or announcing completion. The daemon will deliver it to this")
	fmt.Fprintln(out, "session via prompt_deliver once all agents reach a terminal state.")
}
