package review

// monitor.go — group-completion monitor for async review.
//
// After `prism review` spawns the 5 review-agent sessions and registers a
// group, it starts a detached monitor subprocess (via cmd/monitor_review.go
// using `prism monitor-review`). The monitor polls db.GroupCompleted every 5 s;
// when the group is complete it aggregates results via db.GroupResults,
// formats them with FormatResults, and delivers to the worker via
// `prism prompt`. On delivery failure, it retries with bounded backoff then
// writes the fallback file.
//
// MonitorOpts is passed from the cmd layer; MonitorFunc is the entry-point
// called by the monitor-review subcommand.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// MonitorOpts configures the group-completion monitor.
type MonitorOpts struct {
	// GroupID is the session_groups.group_id to monitor.
	GroupID string
	// WorkerSession is the session name to deliver results to.
	WorkerSession string
	// PRNumber is the PR number string (e.g. "864") — used in the delivery
	// message, retry messages, and fallback file naming.
	PRNumber string
	// Round is the 1-indexed review round number — used in the fallback file name.
	Round int
	// Agents is the ordered list of agents spawned in this round. Used to
	// reconstruct the result ordering when GroupResults is incomplete.
	Agents []Agent
	// AgentSessions maps each agent index to its session name — needed for
	// buildResults to correlate GroupResults data with agent ordering.
	AgentSessions []string
	// DBPath is the path to the prism database. If empty, the default is used.
	DBPath string
	// PollInterval controls how often the monitor checks GroupCompleted.
	// Zero uses the default (5 s).
	PollInterval time.Duration
	// MaxDeliveryRetries is the number of times to retry delivery before writing
	// the fallback file. Zero uses the default (5).
	MaxDeliveryRetries int
	// DeliveryRetryBackoff is the base delay between delivery retries.
	// Zero uses the default (30 s).
	DeliveryRetryBackoff time.Duration
	// Timeout is the maximum time to wait for the group to complete. Zero means
	// no timeout (monitor runs until the group is complete).
	Timeout time.Duration
	// SizeBudget is the maximum inline size (bytes) for full per-agent findings.
	// When the total findings exceed this budget they are written to a temp file
	// and a pointer is included inline. Zero uses the default (20 KB).
	// Can also be overridden via the PRISM_REVIEW_SIZE_BUDGET environment variable.
	SizeBudget int
}

// defaultPollInterval is how often the monitor polls GroupCompleted.
const defaultPollInterval = 5 * time.Second

// defaultMaxDeliveryRetries is how many times to attempt prompt delivery.
const defaultMaxDeliveryRetries = 5

// defaultDeliveryRetryBackoff is the initial backoff between delivery retries.
const defaultDeliveryRetryBackoff = 30 * time.Second

// MonitorFunc is the entry-point for the group-completion monitor.
// It is called by the `prism monitor-review` subcommand after startup.
// It blocks until the group is complete (or the timeout fires), delivers
// results to the worker, and returns. The caller (monitor-review) may
// os.Exit after this returns.
func MonitorFunc(opts MonitorOpts) error {
	if opts.GroupID == "" {
		return fmt.Errorf("monitor-review: group_id is required")
	}
	if opts.WorkerSession == "" {
		return fmt.Errorf("monitor-review: worker_session is required")
	}

	pollInterval := opts.PollInterval
	if pollInterval <= 0 {
		pollInterval = defaultPollInterval
	}
	maxRetries := opts.MaxDeliveryRetries
	if maxRetries <= 0 {
		maxRetries = defaultMaxDeliveryRetries
	}
	retryBackoff := opts.DeliveryRetryBackoff
	if retryBackoff <= 0 {
		retryBackoff = defaultDeliveryRetryBackoff
	}

	dbPath := opts.DBPath
	if dbPath == "" {
		dbPath = defaultDBPath()
	}

	d, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("monitor-review: open db: %w", err)
	}
	defer d.Close()

	fmt.Fprintf(os.Stderr, "[prism monitor-review] watching group %s for PR #%s (worker: %s)\n",
		opts.GroupID, opts.PRNumber, opts.WorkerSession)

	// Set deadline if timeout is specified.
	var deadline time.Time
	if opts.Timeout > 0 {
		deadline = time.Now().Add(opts.Timeout)
	}

	// Poll loop: check GroupCompleted every pollInterval.
	for {
		if !deadline.IsZero() && time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "[prism monitor-review] timeout reached for group %s — delivering partial results\n", opts.GroupID)
			break
		}

		done, groupErr := d.GroupCompleted(opts.GroupID)
		if groupErr != nil {
			fmt.Fprintf(os.Stderr, "[prism monitor-review] warning: GroupCompleted(%s): %v — retrying\n", opts.GroupID, groupErr)
		} else if done {
			fmt.Fprintf(os.Stderr, "[prism monitor-review] group %s complete — aggregating results\n", opts.GroupID)
			break
		}

		time.Sleep(pollInterval)
	}

	// Aggregate results.
	groupData, grErr := d.GroupResults(opts.GroupID)
	if grErr != nil {
		fmt.Fprintf(os.Stderr, "[prism monitor-review] warning: GroupResults(%s): %v — using empty data\n", opts.GroupID, grErr)
		groupData = map[string]db.GroupMemberResult{}
	}

	// Build AgentResult slice, handling "missing" sessions (row deleted mid-review).
	results := buildMonitorResults(opts.Agents, opts.AgentSessions, groupData)

	// Format the delivery message. Pass round and sizeBudget so that
	// overflow-to-file is handled when the total findings exceed the budget.
	output, allPassed := FormatResults(results, opts.PRNumber, opts.Round, opts.SizeBudget)
	deliveryText := buildDeliveryMessage(opts.PRNumber, opts.Round, output, allPassed, groupData, opts.AgentSessions)

	// Deliver to worker via prism prompt with bounded retry.
	deliverErr := deliverWithRetry(opts.WorkerSession, deliveryText, maxRetries, retryBackoff, dbPath)
	if deliverErr != nil {
		// Delivery failed after all retries — write fallback file.
		fallbackPath := fmt.Sprintf("/tmp/prism-review-%s-round-%d-result.md", sanitisePRNumber(opts.PRNumber), opts.Round)
		fmt.Fprintf(os.Stderr, "[prism monitor-review] delivery failed after %d retries — writing fallback to %s\n", maxRetries, fallbackPath)
		writeErr := os.WriteFile(fallbackPath, []byte(deliveryText), 0o644)
		if writeErr != nil {
			fmt.Fprintf(os.Stderr, "[prism monitor-review] error: could not write fallback file %s: %v\n", fallbackPath, writeErr)
		} else {
			fmt.Fprintf(os.Stderr, "[prism monitor-review] fallback file written to %s\n", fallbackPath)
		}
		return fmt.Errorf("monitor-review: delivery failed and fallback written to %s", fallbackPath)
	}

	fmt.Fprintf(os.Stderr, "[prism monitor-review] results delivered to %s\n", opts.WorkerSession)
	return nil
}

// buildMonitorResults constructs AgentResult entries from GroupResults data,
// handling missing sessions (row deleted mid-review) as a special case.
// Missing agents are counted as an error result with state "missing".
func buildMonitorResults(agents []Agent, agentSessions []string, groupData map[string]db.GroupMemberResult) []AgentResult {
	results := make([]AgentResult, len(agents))
	for i, ag := range agents {
		agentSession := ""
		if i < len(agentSessions) {
			agentSession = agentSessions[i]
		}

		mr, ok := groupData[agentSession]
		if !ok || agentSession == "" {
			// Session was deleted mid-review or never registered — count as missing.
			results[i] = AgentResult{
				Agent:   ag,
				Passed:  false,
				Output:  "ERROR: agent session not found in group (possibly deleted mid-review)",
				IsError: true,
			}
			continue
		}

		switch mr.State {
		case "interrupted", "error":
			results[i] = AgentResult{
				Agent:   ag,
				Passed:  false,
				Output:  fmt.Sprintf("ERROR: agent did not complete cleanly (state: %s)", mr.State),
				IsError: true,
			}
		case "finished":
			if mr.LastMessage == "" {
				results[i] = AgentResult{
					Agent:   ag,
					Passed:  false,
					Output:  "ERROR: no output produced",
					IsError: true,
				}
				continue
			}
			text := extractAssistantText(mr.LastMessage)
			passed, kind := AssessPassed(text)
			if !passed && kind == VerdictNone {
				results[i] = AgentResult{
					Agent:   ag,
					Passed:  false,
					Output:  "ERROR: no verdict found in agent output — review output:\n" + text,
					IsError: true,
				}
				continue
			}
			results[i] = AgentResult{
				Agent:  ag,
				Passed: passed,
				Output: text,
			}
		default:
			// Non-terminal state after group says complete — treat as timed out / missing.
			results[i] = AgentResult{
				Agent:   ag,
				Passed:  false,
				Output:  fmt.Sprintf("ERROR: agent in unexpected state %q (may have timed out)", mr.State),
				IsError: true,
			}
		}
	}
	return results
}

// buildDeliveryMessage constructs the prompt text delivered to the worker when
// the review group completes.
func buildDeliveryMessage(prNumber string, round int, formattedResults string, allPassed bool, groupData map[string]db.GroupMemberResult, agentSessions []string) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("## Review complete: PR #%s (round %d)\n\n", prNumber, round))

	if allPassed {
		sb.WriteString("**All 5 review agents passed.** You may proceed with announcing completion.\n\n")
	} else {
		sb.WriteString("**One or more review agents failed.** Fix the blocking issues and re-run `prism review`.\n\n")
	}

	sb.WriteString("### Results\n\n")
	sb.WriteString(formattedResults)

	// Note any missing/deleted sessions.
	var missing []string
	for _, sess := range agentSessions {
		if _, ok := groupData[sess]; !ok {
			missing = append(missing, sess)
		}
	}
	if len(missing) > 0 {
		sb.WriteString(fmt.Sprintf("\n**Note:** %d agent session(s) were not found in the group (possibly deleted mid-review): %s\n",
			len(missing), strings.Join(missing, ", ")))
	}

	return sb.String()
}

// deliverWithRetry attempts to deliver text to workerSession via `prism prompt`,
// retrying up to maxRetries times with exponential backoff starting from baseBackoff.
// dbPath is passed through to deliverPrompt for DB access.
func deliverWithRetry(workerSession, text string, maxRetries int, baseBackoff time.Duration, dbPath string) error {
	var lastErr error
	backoff := baseBackoff

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			fmt.Fprintf(os.Stderr, "[prism monitor-review] delivery attempt %d/%d failed (%v) — retrying in %s\n",
				attempt, maxRetries, lastErr, backoff)
			time.Sleep(backoff)
			backoff *= 2
			if backoff > 5*time.Minute {
				backoff = 5 * time.Minute
			}
		}

		err := deliverPrompt(workerSession, text, dbPath)
		if err == nil {
			return nil
		}
		lastErr = err
	}

	return fmt.Errorf("delivery failed after %d attempts: %w", maxRetries+1, lastErr)
}

// deliverPrompt sends text to workerSession via the prism prompt HTTP API.
// It mirrors the logic in cmd/prompt.go's runPrompt but operates directly
// via the DB + HTTP path (no exec of a subprocess) so the monitor can be
// embedded without spawning child processes.
func deliverPrompt(workerSession, text, dbPath string) error {
	if dbPath == "" {
		dbPath = defaultDBPath()
	}
	d, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer d.Close()

	status, err := d.CurrentStatus(workerSession)
	if err != nil {
		return fmt.Errorf("check session status for %q: %w", workerSession, err)
	}
	if status == nil {
		return fmt.Errorf("session %q not found in DB — worker may have ended", workerSession)
	}
	if status.EndedAt != nil {
		return fmt.Errorf("session %q has ended — cannot deliver review results", workerSession)
	}
	if status.HarnessPort == nil || status.HarnessSessionID == nil {
		return fmt.Errorf("session %q has no harness port or session ID — cannot deliver prompt", workerSession)
	}

	// Build prompt body matching cmd/prompt.go's buildPromptBody.
	body := buildPromptBodyForMonitor(text, status)
	jsonBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal prompt body: %w", err)
	}

	url := fmt.Sprintf("http://localhost:%d/session/%s/prompt_async", *status.HarnessPort, *status.HarnessSessionID)
	req, err := http.NewRequest("POST", url, bytes.NewReader(jsonBytes))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("http request to %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http status %d from %s", resp.StatusCode, url)
	}

	return nil
}

// buildPromptBodyForMonitor constructs the HTTP request body for prompt_async.
// Mirrors cmd/prompt.go buildPromptBody but operates on db.Status directly
// (monitor lives in internal/review, not cmd).
func buildPromptBodyForMonitor(text string, status *db.Status) map[string]any {
	body := map[string]any{
		"parts": []map[string]string{
			{"type": "text", "text": text},
		},
	}

	agentName := status.RootAgentName
	if agentName == nil {
		agentName = status.AgentName
	}
	modelID := status.RootModelID
	if modelID == nil {
		modelID = status.ModelID
	}

	if agentName != nil && modelID != nil {
		body["agent"] = *agentName

		// Split model_id on the first "/" to get providerID and modelID.
		slashIdx := strings.Index(*modelID, "/")
		providerID := *modelID
		modelIDStr := ""
		if slashIdx >= 0 {
			providerID = (*modelID)[:slashIdx]
			modelIDStr = (*modelID)[slashIdx+1:]
		}
		body["model"] = map[string]string{
			"providerID": providerID,
			"modelID":    modelIDStr,
		}
	}

	return body
}

// StartMonitorProcess launches the group-completion monitor as a detached
// subprocess. The monitor runs `prism monitor-review` with the provided opts
// encoded as JSON via a temp file. The subprocess is detached (Setsid) so it
// survives the parent `prism review` process exiting.
//
// The prismBinary parameter is the path to the prism binary. Pass "" to use
// os.Executable() (the current binary) as the default.
func StartMonitorProcess(opts MonitorOpts, prismBinary string) error {
	if prismBinary == "" {
		var err error
		prismBinary, err = os.Executable()
		if err != nil {
			return fmt.Errorf("monitor: resolve prism binary: %w", err)
		}
	}

	// Serialise MonitorOpts to a temp file so we can pass it via flag.
	optsJSON, err := json.Marshal(opts)
	if err != nil {
		return fmt.Errorf("monitor: marshal opts: %w", err)
	}

	tmpFile, err := os.CreateTemp("", "prism-monitor-opts-*.json")
	if err != nil {
		return fmt.Errorf("monitor: create temp opts file: %w", err)
	}
	defer tmpFile.Close()
	if _, err := tmpFile.Write(optsJSON); err != nil {
		return fmt.Errorf("monitor: write opts file: %w", err)
	}

	cmd := exec.Command(prismBinary, "monitor-review", "--opts-file", tmpFile.Name())
	// Detach the process: use setsid so it survives the parent process exiting.
	detachProcess(cmd)
	// Redirect stdout/stderr to log file for diagnostics.
	logPath := fmt.Sprintf("/tmp/prism-monitor-review-%s-round-%d.log", sanitisePRNumber(opts.PRNumber), opts.Round)
	logFile, logErr := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if logErr == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}

	if err := cmd.Start(); err != nil {
		if logFile != nil {
			logFile.Close()
		}
		return fmt.Errorf("monitor: start monitor process: %w", err)
	}
	// Do not wait — the process is detached and runs independently.
	// logFile stays open in the child; Close in parent has no effect on child fd.
	if logFile != nil {
		logFile.Close()
	}
	return nil
}

// ActiveReviewGroupForParent returns the group_id of an in-progress (not yet
// completed) review group for parentSession, or "" when none exists.
// It is used to detect duplicate `prism review` invocations for the same
// parent session.
func ActiveReviewGroupForParent(d *db.DB, parentSession string) (string, error) {
	members, err := d.GroupMembersForParent(parentSession)
	if err != nil {
		return "", fmt.Errorf("active review group: GroupMembersForParent: %w", err)
	}
	if len(members) == 0 {
		return "", nil
	}

	// Group sessions by group_id.
	// A group is "active" when at least one member is not in a terminal state.
	groupStates := make(map[string]bool) // group_id → hasActiveMembers
	for _, m := range members {
		if m.GroupID == nil {
			continue
		}
		gid := *m.GroupID
		if !isTerminalState(m.State) {
			groupStates[gid] = true
		} else if _, exists := groupStates[gid]; !exists {
			groupStates[gid] = false
		}
	}

	for gid, active := range groupStates {
		if active {
			return gid, nil
		}
	}
	return "", nil
}

// ReviewRoundForGroup returns the round number for the given group_id by
// inspecting the session names of its members.
// Returns 0 when the round cannot be determined.
func ReviewRoundForGroup(members []db.Status) int {
	// Member session names have shape <parent>~review-<N>-<agent>.
	// Extract the first N we find.
	for _, m := range members {
		name := m.SessionName
		tilde := strings.Index(name, "~review-")
		if tilde < 0 {
			continue
		}
		suffix := name[tilde+len("~review-"):]
		dash := strings.Index(suffix, "-")
		if dash <= 0 {
			continue
		}
		n := 0
		for _, c := range suffix[:dash] {
			if c < '0' || c > '9' {
				n = 0
				break
			}
			n = n*10 + int(c-'0')
		}
		if n > 0 {
			return n
		}
	}
	return 0
}

// WriteFallbackResult writes the review result to the standard fallback file
// when delivery fails. prNumber is sanitised to prevent path traversal.
// Exported for testing.
func WriteFallbackResult(prNumber string, round int, content string) (string, error) {
	path := fmt.Sprintf("/tmp/prism-review-%s-round-%d-result.md", sanitisePRNumber(prNumber), round)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write fallback result: %w", err)
	}
	return path, nil
}

// MonitorOptsFilePath is the temp-file path used to pass MonitorOpts to the
// monitor subprocess. It is read by the monitor-review subcommand.
// Exported for testability.
func LoadMonitorOptsFromFile(path string) (MonitorOpts, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return MonitorOpts{}, fmt.Errorf("read opts file %q: %w", path, err)
	}
	var opts MonitorOpts
	if err := json.Unmarshal(data, &opts); err != nil {
		return MonitorOpts{}, fmt.Errorf("parse opts file %q: %w", path, err)
	}
	return opts, nil
}

// fallbackFilePath returns the standard fallback result file path.
// prNumber is sanitised to prevent path traversal.
func fallbackFilePath(prNumber string, round int) string {
	return filepath.Join("/tmp", fmt.Sprintf("prism-review-%s-round-%d-result.md", sanitisePRNumber(prNumber), round))
}

// BuildMonitorResultsForTest is an exported wrapper around buildMonitorResults
// for use in external test packages.
func BuildMonitorResultsForTest(agents []Agent, agentSessions []string, groupData map[string]db.GroupMemberResult) []AgentResult {
	return buildMonitorResults(agents, agentSessions, groupData)
}
