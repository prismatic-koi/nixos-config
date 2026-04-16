// Package review implements the prism review execution engine.
//
// prism review <pr-number> spawns N review agent sessions in a dedicated tmux
// session named <parent>~review-N (where N is the 1-indexed round number),
// polls the prism DB until all agents reach the "finished" state, reads their
// last msg_assistant event, and returns aggregated findings to stdout.
//
// Session architecture:
//   - One tmux session per review round: <parent-session>~review-N
//   - One tmux window per review agent within the session
//   - Each agent gets its own sidecar + container mounting the parent worktree read-only
//   - No new worktree is created
//
// The ~ separator in the session name is used by the dashboard for depth-2
// child detection (review sessions appear indented under their parent branch).
package review

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// KillSessionPrefix kills all tmux sessions whose names start with the given
// prefix. Used to clean up all ~review-* sessions for a parent.
func KillSessionPrefix(prefix string) {
	out, err := tmux.Run("list-sessions", "-F", "#{session_name}")
	if err != nil {
		return
	}
	for _, name := range strings.Split(out, "\n") {
		name = strings.TrimSpace(name)
		if name != "" && strings.HasPrefix(name, prefix) {
			_ = tmux.KillSession(name)
		}
	}
}

// NextRoundNumber returns the next round number for the given parent session.
// It queries the DB for all round sessions (not live tmux sessions) so that
// the count is accurate even after previous rounds have been cleaned up.
// Returns 1 when no prior rounds exist.
func NextRoundNumber(d *db.DB, parentSession string) int {
	prefix := parentSession + "~review-"
	rows, err := d.AllStatusesWithPrefix(prefix)
	if err != nil {
		return 1
	}
	max := 0
	for _, row := range rows {
		// Only count round-level sessions (no further ~ after the round number).
		suffix := strings.TrimPrefix(row.SessionName, prefix)
		// suffix should be a pure integer (e.g. "1", "2") for round sessions.
		// Agent sub-sessions have a suffix like "1~review", which won't parse.
		if n, err := strconv.Atoi(suffix); err == nil && n > max {
			max = n
		}
	}
	return max + 1
}

// Agent describes a single review agent to run.
type Agent struct {
	// Name is the agent identifier, e.g. "review".
	Name string
	// OpencodeName is the opencode --agent flag value, e.g. "review".
	OpencodeName string
}

// DefaultAgents returns the default set of review agents for the opencode harness.
func DefaultAgents() []Agent {
	return []Agent{
		{Name: "review", OpencodeName: "review"},
	}
}

// AgentsByName filters the agents slice to only those whose Name is in the
// allowedNames set. Returns an error if any name in allowedNames does not exist
// in agents.
func AgentsByName(agents []Agent, allowedNames []string) ([]Agent, error) {
	available := make(map[string]Agent, len(agents))
	for _, a := range agents {
		available[a.Name] = a
	}
	var result []Agent
	var unknown []string
	for _, name := range allowedNames {
		if a, ok := available[name]; ok {
			result = append(result, a)
		} else {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("unknown agent name(s): %s\navailable: %s",
			strings.Join(unknown, ", "),
			strings.Join(agentNames(agents), ", "),
		)
	}
	return result, nil
}

func agentNames(agents []Agent) []string {
	names := make([]string, len(agents))
	for i, a := range agents {
		names[i] = a.Name
	}
	return names
}

// AgentResult holds the outcome for a single review agent.
type AgentResult struct {
	Agent   Agent
	Passed  bool   // true = passed, false = failed / errored
	Output  string // last assistant message text, or error description
	IsError bool   // true = infrastructure/timeout failure
}

// Opts configures a review run.
type Opts struct {
	// PRNumber is the pull request number to review.
	PRNumber string
	// ParentSession is the prism session name of the calling worker/coordinator.
	ParentSession string
	// Worktree is the absolute path to the parent session's worktree.
	Worktree string
	// Agents is the list of review agents to spawn.
	Agents []Agent
	// Harness is the runtime harness to use ("opencode").
	Harness string
	// Timeout is the per-agent maximum wait time.
	Timeout time.Duration
	// Keep: if true, the review session is not killed after completion.
	Keep bool
	// DBPath is the path to the prism database. If empty, the default is used.
	DBPath string
	// PluginHostPath is the path to the opencode plugin file.
	PluginHostPath string
	// ConfigContent is the JSON blob for the container's opencode.json config.
	ConfigContent string
	// ContainerMode: when true, each agent runs in its own container.
	ContainerMode bool
}

// Run executes the review. It returns the aggregated results and a boolean
// indicating whether all agents passed.
//
// On signal (SIGTERM/SIGINT), the caller is expected to kill the review session
// using the session name returned via the onSessionCreated callback.
func Run(ctx context.Context, opts Opts, onSessionCreated func(sessionName string)) ([]AgentResult, error) {
	if opts.ParentSession == "" {
		return nil, fmt.Errorf("parent session name is required")
	}

	// Resolve worktree path.
	worktree := opts.Worktree
	if worktree == "" {
		return nil, fmt.Errorf("worktree path is required")
	}

	// Open DB.
	dbPath := opts.DBPath
	if dbPath == "" {
		dbPath = defaultDBPath()
	}
	d, err := db.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	defer d.Close()

	// Determine round number from DB BEFORE killing previous sessions.
	// Using DB-based counting ensures correctness even after prior sessions
	// are killed (tmux sessions disappear, but DB rows persist).
	round := NextRoundNumber(d, opts.ParentSession)
	reviewSession := fmt.Sprintf("%s~review-%d", opts.ParentSession, round)

	// Kill any previous review sessions for this parent.
	reviewPrefix := opts.ParentSession + "~review-"
	KillSessionPrefix(reviewPrefix)

	// Create the review tmux session.
	if err := tmux.NewSessionDetached(reviewSession, worktree); err != nil {
		return nil, fmt.Errorf("create review session %q: %w", reviewSession, err)
	}

	if onSessionCreated != nil {
		onSessionCreated(reviewSession)
	}

	// Insert the round session into the DB so it shows up in checkin.
	// The repo field is derived from the parent session name.
	repo := deriveRepo(opts.ParentSession)
	if err := d.UpsertStatus(reviewSession, repo, worktree, "idle", nil, nil); err != nil {
		fmt.Fprintf(os.Stderr, "[prism review] warning: upsert round session status: %v\n", err)
	}

	agents := opts.Agents
	if len(agents) == 0 {
		agents = DefaultAgents()
	}

	// Spawn each agent in its own tmux window within the review session.
	agentSessions := make([]string, len(agents))
	for i, ag := range agents {
		// Agent sub-sessions are named <reviewSession>~<agentName>.
		// They have a second ~ in the branch component, making them depth-2+.
		agentSession := fmt.Sprintf("%s~%s", reviewSession, ag.Name)
		agentSessions[i] = agentSession

		// Seed agent_status for the agent session.
		if err := d.UpsertStatus(agentSession, repo, worktree, "idle", nil, nil); err != nil {
			return nil, fmt.Errorf("seed status for %s: %w", ag.Name, err)
		}

		// Allocate a port for this agent session.
		port, portErr := d.AllocatePort(agentSession)
		if portErr != nil {
			return nil, fmt.Errorf("allocate port for %s: %w", ag.Name, portErr)
		}

		// Build the prompt for the review agent.
		prompt := buildReviewPrompt(opts.PRNumber)

		// Start the sidecar for this agent.
		sidecarOpts := session.StartSidecarOpts{
			Port:           port,
			ContainerMode:  opts.ContainerMode,
			AgentRole:      ag.Name,
			Worktree:       worktree,
			PluginHostPath: opts.PluginHostPath,
			InitialPrompt:  prompt,
			ConfigContent:  opts.ConfigContent,
		}
		if sidecarErr := session.StartSidecarWithOpts(agentSession, sidecarOpts); sidecarErr != nil {
			fmt.Fprintf(os.Stderr, "[prism review] warning: could not start sidecar for %s: %v\n", ag.Name, sidecarErr)
		}

		// Create the agent window within the review session.
		agentCmd := buildAgentCommand(ag, agentSession, port, prompt, opts.ContainerMode)
		if err := createAgentWindow(reviewSession, i, ag.Name, worktree, agentCmd, port, agentSession, opts.ContainerMode); err != nil {
			return nil, fmt.Errorf("create window for %s: %w", ag.Name, err)
		}
	}

	// Remove the initial blank window (window 0) that was created with the session.
	// All agents are in windows 1..N.
	_, _ = tmux.Run("kill-window", "-t", reviewSession+":0")

	// Poll DB until all agents finish or timeout.
	results, pollErr := pollAgents(ctx, d, agents, agentSessions, opts.Timeout)

	// Mark round session as finished in the DB.
	_ = d.UpsertStatus(reviewSession, repo, worktree, "finished", nil, nil)

	// Clean up sidecar processes.
	for _, agentSession := range agentSessions {
		session.KillSidecar(agentSession)
	}

	// Kill the review session unless --keep.
	if !opts.Keep {
		_ = tmux.KillSession(reviewSession)
		// Clean up DB entries for agent sub-sessions.
		for _, agentSession := range agentSessions {
			cleanupAgentSession(d, agentSession)
		}
		// Mark the round session as ended.
		_ = d.SetEnded(reviewSession)
	}

	return results, pollErr
}

// buildReviewPrompt returns the initial prompt string for a review agent.
func buildReviewPrompt(prNumber string) string {
	return fmt.Sprintf("Review PR #%s. Run `gh pr view %s` and `gh pr diff %s` to get the full diff. Check the linked issue for acceptance criteria and validate each one. Report your findings clearly.", prNumber, prNumber, prNumber)
}

// buildAgentCommand returns the opencode command string for a review agent window.
// agentSession is the full session name (e.g. "nixos-config@feature~review-1~review"),
// used as PRISM_SESSION_NAME so the plugin correctly identifies the DB row.
func buildAgentCommand(ag Agent, agentSession string, port int, prompt string, containerMode bool) string {
	if containerMode {
		return fmt.Sprintf("opencode attach http://localhost:%d", port)
	}
	escapedPrompt := shellQuote(prompt)
	return fmt.Sprintf("PRISM_SESSION_NAME=%s opencode --agent %s --port %d --hostname 127.0.0.1 --prompt %s",
		shellQuote(agentSession), ag.OpencodeName, port, escapedPrompt)
}

// createAgentWindow creates a tmux window for a review agent.
// In container mode it prepends a readiness wait like the standard session setup.
func createAgentWindow(reviewSession string, idx int, windowName, worktree, agentCmd string, port int, agentSession string, containerMode bool) error {
	cmd := agentCmd
	if containerMode && port != 0 {
		readyPath, pathErr := session.SidecarReadyPath(agentSession)
		sidPath, sidErr := session.SidecarSessionPath(agentSession)
		if pathErr == nil {
			_ = os.Remove(readyPath)
			if sidErr != nil {
				sidPath = ""
			} else {
				_ = os.Remove(sidPath)
			}
			cmd = buildReadinessWaitCmd(readyPath, sidPath, agentCmd)
		}
	}
	// Windows are 1-indexed: window 0 is the initial blank shell.
	return tmux.NewWindow(reviewSession, idx+1, windowName, worktree, cmd)
}

// buildReadinessWaitCmd mirrors session.buildReadinessWaitCmd (unexported).
func buildReadinessWaitCmd(readyPath, sidPath, attachCmd string) string {
	return fmt.Sprintf(
		`i=0; while [ ! -f %s ] && [ $i -lt 240 ]; do sleep 0.5; i=$((i+1)); done; `+
			`if [ ! -f %s ]; then `+
			`echo "prism: container did not become ready within 120s" >&2; exit 1; `+
			`fi; `+
			`if [ -f %s ]; then _sid=$(cat %s); %s -s "$_sid"; else %s; fi`,
		shellQuote(readyPath), shellQuote(readyPath),
		shellQuote(sidPath), shellQuote(sidPath), attachCmd, attachCmd,
	)
}

// pollAgents polls the DB until all agents reach "finished" or the timeout expires.
func pollAgents(ctx context.Context, d *db.DB, agents []Agent, agentSessions []string, timeout time.Duration) ([]AgentResult, error) {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}

	deadline := time.Now().Add(timeout)
	finished := make([]bool, len(agents))
	timedOut := make([]bool, len(agents))

	const pollInterval = 2 * time.Second

	for {
		allDone := true
		for i, agentSession := range agentSessions {
			if finished[i] || timedOut[i] {
				continue
			}
			status, err := d.CurrentStatus(agentSession)
			if err != nil {
				// DB error — treat as still running.
				allDone = false
				continue
			}
			if status != nil && isTerminalState(status.State) {
				finished[i] = true
			} else if time.Now().After(deadline) {
				timedOut[i] = true
			} else {
				allDone = false
			}
		}

		// Check context cancellation.
		select {
		case <-ctx.Done():
			// Build partial results with all non-finished agents as timed out.
			return buildResults(agents, agentSessions, d, timedOut, timeout, true), ctx.Err()
		default:
		}

		if allDone {
			break
		}

		if time.Now().After(deadline) {
			// Mark all remaining as timed out.
			for i := range agents {
				if !finished[i] {
					timedOut[i] = true
				}
			}
			break
		}

		time.Sleep(pollInterval)
	}

	return buildResults(agents, agentSessions, d, timedOut, timeout, false), nil
}

// isTerminalState returns true if the state is considered terminal (finished or error).
func isTerminalState(state string) bool {
	switch state {
	case "finished", "interrupted", "error":
		return true
	}
	return false
}

// buildResults constructs AgentResult entries from polling outcomes.
func buildResults(agents []Agent, agentSessions []string, d *db.DB, timedOut []bool, timeout time.Duration, cancelled bool) []AgentResult {
	results := make([]AgentResult, len(agents))
	for i, ag := range agents {
		agentSession := agentSessions[i]
		if timedOut[i] || cancelled {
			results[i] = AgentResult{
				Agent:   ag,
				Passed:  false,
				Output:  fmt.Sprintf("ERROR: timed out after %s", formatDuration(timeout)),
				IsError: true,
			}
			continue
		}

		// Read last msg_assistant event.
		events, err := d.QueryEvents(agentSession, 1, nil, nil, []string{"msg_assistant"})
		if err != nil || len(events) == 0 {
			// Check if there was a crash (state is interrupted/error without output).
			status, _ := d.CurrentStatus(agentSession)
			if status != nil && (status.State == "interrupted" || status.State == "error") {
				results[i] = AgentResult{
					Agent:   ag,
					Passed:  false,
					Output:  fmt.Sprintf("ERROR: agent crashed (state: %s)", status.State),
					IsError: true,
				}
			} else {
				results[i] = AgentResult{
					Agent:   ag,
					Passed:  false,
					Output:  "ERROR: no output produced",
					IsError: true,
				}
			}
			continue
		}

		// Extract text from the last msg_assistant event.
		text := extractAssistantText(events[len(events)-1].Payload)
		passed := assessPassed(text)

		results[i] = AgentResult{
			Agent:  ag,
			Passed: passed,
			Output: text,
		}
	}
	return results
}

// extractAssistantText parses the text field from a msg_assistant payload.
func extractAssistantText(payload string) string {
	var p struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err == nil && p.Text != "" {
		return p.Text
	}
	return payload
}

// assessPassed heuristically determines whether a review agent passed.
// A review "passes" when the agent found no blocking issues. We look for
// common patterns that indicate a clean review.
func assessPassed(text string) bool {
	lower := strings.ToLower(text)
	// Explicit failure indicators.
	failPhrases := []string{
		"please fix",
		"needs to be fixed",
		"must be fixed",
		"blocking issue",
		"this is a bug",
		"error found",
		"security issue",
		"vulnerability",
	}
	for _, phrase := range failPhrases {
		if strings.Contains(lower, phrase) {
			return false
		}
	}
	// If the text contains ✗ it likely has failures.
	if strings.Contains(text, "✗") {
		return false
	}
	return true
}

// cleanupAgentSession cleans up the DB state for a completed agent sub-session.
func cleanupAgentSession(d *db.DB, agentSession string) {
	_ = d.ReleasePort(agentSession)
	_ = d.SetEnded(agentSession)
	_ = d.PurgeBusMessages(agentSession)
}

// FormatResults formats the aggregated results as a human-readable report.
// Returns the formatted string and a boolean indicating whether all passed.
func FormatResults(results []AgentResult, prNumber string) (string, bool) {
	var sb strings.Builder
	allPassed := true
	var failed []string

	for _, r := range results {
		if r.Passed {
			sb.WriteString(fmt.Sprintf("✓ %-20s passed\n", r.Agent.Name))
		} else {
			allPassed = false
			failed = append(failed, r.Agent.Name)
			if r.IsError {
				sb.WriteString(fmt.Sprintf("✗ %-20s %s\n", r.Agent.Name, r.Output))
			} else {
				// Content failure: include the output.
				sb.WriteString(fmt.Sprintf("✗ %-20s\n", r.Agent.Name))
				// Indent the output.
				for _, line := range strings.Split(r.Output, "\n") {
					sb.WriteString("  " + line + "\n")
				}
			}
		}
	}

	if !allPassed {
		sb.WriteString(fmt.Sprintf("\n%d agent(s) failed. Retry: prism review %s --only %s\n",
			len(failed), prNumber, strings.Join(failed, ",")))
	}

	return sb.String(), allPassed
}

// LookupParentSession looks up the parent session from the environment or DB.
// When called from inside a container, PRISM_SESSION_NAME is set to the
// agent's session name. We use that directly (the worker's session name).
func LookupParentSession() string {
	// Try PRISM_SESSION_NAME first (set when running inside a container/agent).
	if s := os.Getenv("PRISM_SESSION_NAME"); s != "" {
		return s
	}
	// Fall back to TMUX current session.
	sess, err := tmux.CurrentSession()
	if err != nil {
		return ""
	}
	return sess
}

// IsRoundSession returns true if the given session name is a review round
// session (not an agent sub-session). Round sessions have exactly one ~ in
// the branch component and the suffix after ~review- is a pure integer.
// Agent sub-sessions have a second ~ (e.g. "~review-1~review").
func IsRoundSession(sessionName, parentSession string) bool {
	prefix := parentSession + "~review-"
	if !strings.HasPrefix(sessionName, prefix) {
		return false
	}
	suffix := strings.TrimPrefix(sessionName, prefix)
	_, err := strconv.Atoi(suffix)
	return err == nil
}

// defaultDBPath returns the default prism DB path.
func defaultDBPath() string {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, _ := os.UserHomeDir()
		stateHome = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(stateHome, "prism", "prism.db")
}

// deriveRepo returns the repo portion of a session name (before "@").
func deriveRepo(sessionName string) string {
	if idx := strings.Index(sessionName, "@"); idx >= 0 {
		return sessionName[:idx]
	}
	return sessionName
}

// shellQuote wraps s in single quotes, escaping any single quotes within.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// formatDuration formats a duration as "Xm" or "Xs" for display.
func formatDuration(d time.Duration) string {
	if d >= time.Minute {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

// KillReviewSessionsForParent kills all ~review-* sessions for the given parent.
// This is the public API used by cleanup.go.
func KillReviewSessionsForParent(parentSession string) {
	prefix := parentSession + "~review-"
	KillSessionPrefix(prefix)
}
