// Package quick implements the "prism quick pr" command.
//
// It generates a PR description from staged git changes by invoking the
// `pi` binary (with the anthropic-oauth extension for auth), then creates
// a branch, commits, pushes, and opens a GitHub PR — all from main.
// On success it switches back to main and opens the PR in the system browser.
package quick

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/prismatic-koi/prism/internal/config"
)

// piExecTimeout bounds how long the `pi` subprocess invoked by
// realPiExec is allowed to run before it is killed. `pi` is the only
// subprocess in the quick pr path with no natural completion signal from
// the caller's side, so it is the only one that needs an explicit deadline.
//
// This is a package-level var, not a const, solely so tests can shorten
// it to exercise the real timeout path (context.DeadlineExceeded, process
// kill) without a real 3-minute wait — mirroring the parent-deadline seam
// used by mergequeue's execGH test. Production code never reassigns it.
var piExecTimeout = 3 * time.Minute

// piEnvBlockList names the environment variables stripped from the `pi`
// subprocess's environment. These are set by prism when the CURRENT
// process is itself running as an agent session under prism, and if
// forwarded unchanged to a nested `pi` invocation, they make the nested
// process believe it IS that session: it would try to
// attach to the calling session's harness pipe and session file, and its
// stdin is /dev/null, so any prompt it renders as a result can never be
// answered.
var piEnvBlockList = []string{
	"PI_SESSION_ID",
	"PI_SESSION_FILE",
	"PI_CODING_AGENT",
	"PI_CODING_AGENT_DIR",
	"PRISM_HARNESS_PIPE",
}

// filteredEnv returns os.Environ() with every variable named in
// piEnvBlockList removed, preserving every other variable (including
// PATH, so `pi` itself and its own tool invocations keep resolving).
func filteredEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		blocked := false
		for _, name := range piEnvBlockList {
			if strings.HasPrefix(kv, name+"=") {
				blocked = true
				break
			}
		}
		if !blocked {
			out = append(out, kv)
		}
	}
	return out
}

// titleMaxLen is the maximum PR title length enforced by the existing
// convention. Pi is instructed to respect this in the system prompt; we
// truncate (and warn) as a defensive fallback if the model overruns.
const titleMaxLen = 72

// ── Test seams ─────────────────────────────────────────────────────────────
//
// The package-level function vars below follow the investigateSpawnSessionFn
// seam pattern (cmd/investigate.go). Tests override these to drive Run()
// without actually exec'ing pi / git / gh / a browser.
//
// piLookPathFn   — pre-flight check that `pi` is on PATH.
// piExecFn       — runs `pi <args>` with the given stdin and returns the
//                  captured stdout/stderr plus run error.
// gitRunFn       — runs `git <args>` streaming to the user's stdout/stderr.
// gitOutputFn    — runs `git <args>` capturing stdout.
// ghRunFn        — runs `gh <args>` streaming to the user's stdout/stderr.
// ghOutputFn     — runs `gh <args>` capturing stdout.
// openBrowserFn  — opens a URL in the system browser.

type piResult struct {
	stdout string
	stderr string
	err    error
}

var (
	piLookPathFn  = realPiLookPath
	piExecFn      = realPiExec
	gitRunFn      = realGitRun
	gitOutputFn   = realGitOutput
	ghRunFn       = realGhRun
	ghOutputFn    = realGhOutput
	openBrowserFn = realOpenBrowser
)

func realPiLookPath() error {
	if _, err := exec.LookPath("pi"); err != nil {
		return fmt.Errorf("pi binary not found on PATH (install the pi CLI or add it to PATH): %w", err)
	}
	return nil
}

func realPiExec(args []string, stdin string) piResult {
	ctx, cancel := context.WithTimeout(context.Background(), piExecTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "pi", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}

	// Explicit environment: strip the PI_*/PRISM_HARNESS_PIPE vars that
	// would otherwise be inherited from a calling prism agent session
	// and keep everything else.
	cmd.Env = filteredEnv()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	// Tee stderr to the user's terminal (so `pi` progress is visible while
	// it runs) and to the capture buffer (so callers can still inspect it
	// on error) at the same time.
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("pi subprocess timed out after %s: %w", piExecTimeout, err)
	}
	return piResult{
		stdout: stdout.String(),
		stderr: stderr.String(),
		err:    err,
	}
}

func realGitRun(args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func realGitOutput(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func realGhRun(args ...string) error {
	cmd := exec.Command("gh", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func realGhOutput(args ...string) (string, error) {
	cmd := exec.Command("gh", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func realOpenBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	default: // linux and others
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

// ── System prompt ──────────────────────────────────────────────────────────
//
// Defines the role, codifies the output structure, includes worked examples
// for the "quick pr" tactical use case (NOT long-form worker-agent PRs), and
// emphasises "why" over diff-recital.

const quickPRSystemPrompt = `You are writing a tactical pull request title and body for a single-commit change.

ROLE & SCOPE

The user is invoking ` + "`prism quick pr`" + ` — a fast path for small, focused changes:
dotfile tweaks, README edits, version bumps, small refactors, config tweaks,
comment fixes, lint-rule adjustments. This is NOT the long-form worker-agent PR
style; do NOT produce sections like "## Summary", "## Failure chain", or
"## Acceptance Criteria". Plain, tight, conversational prose only.

OUTPUT FORMAT (STRICT)

Respond with exactly ONE JSON object and nothing else — no prose preamble, no
markdown code fences, no trailing commentary. The shape is:

{"title": "<title>", "body": "<body>"}

If you wrap the JSON in markdown fences or add explanatory text, the caller's
parser will fail. JSON only.

TITLE RULES

- Imperative mood: "Fix...", "Add...", "Drop...", "Bump...", "Switch...".
- ≤ 72 characters. Hard limit.
- No trailing period.
- No conventional-commit prefix (no "feat:", "fix:", "chore:").
- Lead with the action and the affected component or behaviour.

BODY RULES

- Plain text. No markdown headers. Backticks for code identifiers are fine.
- Focus on WHY the change is needed: what problem does it solve, what was the
  motivating observation. Do NOT recite the diff file-by-file — the reviewer
  can read the diff themselves.
- 1–4 sentences for normal changes. May be empty ("") for trivial changes
  (typo fixes, single-character corrections) where the title is fully
  self-explanatory.
- For purely mechanical changes (version bumps, dependency updates), keep
  it brief — one sentence naming the upstream change is plenty.

REPO AWARENESS

You have access to the project's AGENTS.md / CLAUDE.md / CONTRIBUTING.md if
they exist in the working directory. Use them to match the project's
conventions (commit style, terminology, focus areas) — but only where it
adds value. Do not parrot the AGENTS.md verbatim.

WORKED EXAMPLES

Example 1 — small dotfile tweak:
Diff context: a one-line change in a neovim config replacing
` + "`set number relativenumber`" + ` with ` + "`set number nornu`" + `.
Output:
{"title":"Disable relative line numbers in neovim","body":"Relative numbers were adding visual noise during long reading sessions and the jump-by-count workflow is now handled by leap.nvim, so rnu is redundant."}

Example 2 — version bump:
Diff context: package.json updates ` + "`\"react\": \"^18.2.0\"`" + ` to ` + "`\"react\": \"^18.3.1\"`" + `.
Output:
{"title":"Bump react to 18.3.1","body":"Picks up the upstream useId fix and the deprecation-warning suppression on Suspense boundaries. No app-side changes needed."}

Example 3 — typo fix:
Diff context: a README sentence changes "recieve" to "receive".
Output:
{"title":"Fix \"recieve\" typo in README","body":""}

Now produce the JSON response for the diff the user is about to send.`

// ── Run ────────────────────────────────────────────────────────────────────

// Run executes the quick pr workflow.
func Run() error {
	// ── Pre-flight checks ──────────────────────────────────────────────────

	// Pi-on-PATH check is FIRST so that a missing pi binary fails cleanly
	// before any git/gh side effects.
	if err := piLookPathFn(); err != nil {
		return fmt.Errorf("quick pr: %w", err)
	}

	branch, err := currentBranch()
	if err != nil {
		return err
	}
	if branch != "main" {
		return fmt.Errorf(
			"not on main branch (currently on %q)\n"+
				"hint: switch with: git checkout main", branch,
		)
	}

	staged, err := stagedFiles()
	if err != nil {
		return err
	}
	if len(staged) == 0 {
		return fmt.Errorf(
			"no staged files\n" +
				"hint: stage changes with: git add <files>",
		)
	}

	// ── Load configuration ─────────────────────────────────────────────────

	pf, err := config.LoadProfiles()
	if err != nil {
		return fmt.Errorf("quick pr: %w", err)
	}

	qp, ok := pf.QuickProfiles["pr"]
	if !ok {
		return fmt.Errorf("quick pr: no 'pr' entry in quick_profiles — rebuild system config")
	}
	if qp.Model == "" {
		return fmt.Errorf("quick pr: quick_profiles.pr.model is empty — rebuild system config")
	}

	// ── Git diff analysis ──────────────────────────────────────────────────

	diff, err := stagedDiff()
	if err != nil {
		return err
	}

	// ── Generate PR description via pi ─────────────────────────────────────

	title, body, err := generateDescription(qp, diff)
	if err != nil {
		return err
	}

	// Defensive title truncation: pi is instructed to respect the 72-char
	// limit, but warn-and-trim if it overruns.
	if len(title) > titleMaxLen {
		fmt.Fprintf(os.Stderr,
			"warning: pi returned a %d-char title (max %d) — truncating\n",
			len(title), titleMaxLen)
		title = title[:titleMaxLen]
	}

	if title == "" {
		return fmt.Errorf("quick pr: pi returned an empty title")
	}

	// ── Git operations ─────────────────────────────────────────────────────

	newBranch := fmt.Sprintf("quick/pr-%d", time.Now().Unix())
	fmt.Printf("Creating branch %s...\n", newBranch)

	if err := gitRunFn("switch", "-c", newBranch); err != nil {
		return fmt.Errorf("git switch -c: %w", err)
	}

	// Ensure we switch back to main even if something fails after this point.
	defer func() {
		_ = gitRunFn("switch", "main")
	}()

	// Build commit args: always include title; include body as a separate -m
	// flag if non-empty. Git joins multiple -m flags with blank lines, giving
	// a proper "subject\n\nbody" commit. For single-commit PRs this flows
	// straight into GitHub's squash merge extended description.
	commitArgs := []string{"commit", "-m", title}
	if body != "" {
		commitArgs = append(commitArgs, "-m", body)
	}
	if err := gitRunFn(commitArgs...); err != nil {
		return fmt.Errorf("git commit: %w", err)
	}

	fmt.Println("Pushing branch...")
	if err := gitRunFn("push", "-u", "origin", newBranch); err != nil {
		return fmt.Errorf("git push: %w", err)
	}

	// ── Create GitHub PR ───────────────────────────────────────────────────

	fmt.Println("Creating PR...")
	prArgs := []string{"pr", "create", "--title", title}
	if body != "" {
		prArgs = append(prArgs, "--body", body)
	} else {
		prArgs = append(prArgs, "--body", "")
	}
	if err := ghRunFn(prArgs...); err != nil {
		return fmt.Errorf("gh pr create: %w", err)
	}

	// ── Fetch PR URL ───────────────────────────────────────────────────────

	prURL, err := ghOutputFn("pr", "view", "--json", "url", "-q", ".url")
	if err != nil {
		return fmt.Errorf("gh pr view: %w", err)
	}
	prURL = strings.TrimSpace(prURL)

	fmt.Printf("PR created: %s\n", prURL)

	// ── Switch back to main ────────────────────────────────────────────────
	// The deferred switch handles the actual switch; we cancel it here so we
	// can print a clean message, then let the defer run anyway (no-op on main).
	if err := gitRunFn("switch", "main"); err != nil {
		return fmt.Errorf("git switch main: %w", err)
	}

	// ── Open browser ───────────────────────────────────────────────────────

	if prURL != "" {
		if err := openBrowserFn(prURL); err != nil {
			// Non-fatal: just print the URL so the user can open it manually.
			fmt.Fprintf(os.Stderr, "warning: could not open browser: %v\n", err)
		}
	}

	return nil
}

// ── Helpers ────────────────────────────────────────────────────────────────

func currentBranch() (string, error) {
	out, err := gitOutputFn("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %w", err)
	}
	return strings.TrimSpace(out), nil
}

func stagedFiles() ([]string, error) {
	out, err := gitOutputFn("diff", "--cached", "--name-only")
	if err != nil {
		return nil, fmt.Errorf("git diff --cached: %w", err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

func stagedDiff() (string, error) {
	out, err := gitOutputFn("diff", "--cached")
	if err != nil {
		return "", fmt.Errorf("git diff --cached: %w", err)
	}
	return out, nil
}

// generateDescription invokes pi to produce a PR title and body for the
// given staged diff. It uses pi's `--print --mode json` non-interactive
// path with `--no-tools --no-skills`, leaving AGENTS.md auto-discovery
// enabled (no `--no-context-files`) for repo-style awareness.
//
// Output extraction strategy: JSON-in-JSON.
//
//  1. Pi emits NDJSON (one JSON object per line) when --mode json is set.
//  2. We scan for the final `agent_end` event; its `messages` array contains
//     the assistant's full reply.
//  3. The assistant text is itself a JSON object {"title":"...","body":"..."}
//     per the system prompt, which we parse to extract the two fields.
//
// Rejected alternative: fenced sentinels like <title>...</title>. JSON is
// chosen because Sonnet 4.6 is highly reliable with structured JSON output
// and the parse path is unambiguous (no regex on free-form text).
func generateDescription(qp config.QuickProfile, diff string) (title, body string, err error) {
	fmt.Println("Generating PR description with pi...")
	// Pi's `--print --mode json` non-interactive path. Flags:
	//   --print           one-shot completion
	//   --mode json       NDJSON output for structured parse
	//   --no-tools        no tool calls
	//   --no-skills       no skill loading
	//   --system-prompt   our role/format/example prompt
	//   --model/--provider — drives the anthropic-oauth route via pi
	//
	// AGENTS.md auto-discovery is intentionally left enabled (we do NOT
	// pass --no-context-files), so pi picks up the repo's conventions.
	provider := "anthropic" // single-source-of-truth for the anthropic-oauth route
	args := []string{
		"--print",
		"--mode", "json",
		"--no-tools",
		"--no-skills",
		"--system-prompt", quickPRSystemPrompt,
		"--model", qp.Model,
		"--provider", provider,
	}

	// User message: a short framing line plus the diff. Passed as the
	// final positional arg so it becomes the initial user message body.
	userMsg := "Generate a PR title and body in the required JSON shape for the following staged git diff:\n\n" + diff
	args = append(args, userMsg)

	res := piExecFn(args, "")
	if res.err != nil {
		// Surface pi's stderr verbatim so OAuth errors, model errors, etc.
		// reach the user; wrap with a `quick pr:` prefix for clarity.
		stderr := strings.TrimSpace(res.stderr)
		if stderr == "" {
			return "", "", fmt.Errorf("quick pr: pi exec failed: %w", res.err)
		}
		return "", "", fmt.Errorf("quick pr: pi exec failed: %w\npi stderr:\n%s", res.err, stderr)
	}

	return extractTitleBody(res.stdout)
}

// extractTitleBody parses pi's NDJSON --mode json stdout and pulls the
// title/body from the assistant's reply (which is itself a JSON object
// per the system prompt).
//
// Returns a clear error including a snippet of the raw stdout if any
// stage of the parse fails.
func extractTitleBody(piStdout string) (title, body string, err error) {
	if strings.TrimSpace(piStdout) == "" {
		return "", "", fmt.Errorf("quick pr: pi returned empty stdout")
	}

	assistantText, err := assistantTextFromNDJSON(piStdout)
	if err != nil {
		return "", "", fmt.Errorf("quick pr: %w\npi stdout (truncated):\n%s", err, truncate(piStdout, 2000))
	}

	// The assistant text is supposed to be JSON; strip any surrounding
	// markdown fences (defensive — the system prompt forbids them, but
	// models occasionally add them anyway).
	jsonText := stripCodeFences(strings.TrimSpace(assistantText))

	var parsed struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := json.Unmarshal([]byte(jsonText), &parsed); err != nil {
		return "", "", fmt.Errorf(
			"quick pr: could not parse pi assistant text as JSON {title,body}: %w\nassistant text:\n%s\npi stdout (truncated):\n%s",
			err, truncate(assistantText, 2000), truncate(piStdout, 2000),
		)
	}

	title = strings.TrimSpace(parsed.Title)
	body = strings.TrimSpace(parsed.Body)
	return title, body, nil
}

// piContentBlock is one block of an assistant message's content array.
type piContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type piMessage struct {
	Role    string           `json:"role"`
	Content []piContentBlock `json:"content"`
}

type piEnvelope struct {
	Type     string      `json:"type"`
	Messages []piMessage `json:"messages,omitempty"`
	Message  *piMessage  `json:"message,omitempty"`
}

// assistantTextFromNDJSON walks pi's NDJSON --mode json stream and returns
// the assistant's final reply text. It prefers the `agent_end` event (which
// carries the full message history) and falls back to the final `turn_end`
// or assistant `message_end` event if `agent_end` is absent.
func assistantTextFromNDJSON(stream string) (string, error) {
	// Event shapes (minimal — we ignore everything else pi emits):
	//   {"type":"agent_end","messages":[ {role,content:[{type:"text",text}] }, ... ]}
	//   {"type":"turn_end","message":{role:"assistant",content:[{type:"text",text}]}}
	//   {"type":"message_end","message":{role:"assistant",content:[{type:"text",text}]}}
	var (
		fromAgentEnd     string
		fromTurnEnd      string
		fromMessageEnd   string
		anyEventObserved bool
	)

	for _, line := range strings.Split(stream, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var env piEnvelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			// Non-JSON lines are tolerated — pi may emit ordinary log lines
			// in some modes. Skip them.
			continue
		}
		anyEventObserved = true
		switch env.Type {
		case "agent_end":
			// Walk messages in reverse for the last assistant message.
			for i := len(env.Messages) - 1; i >= 0; i-- {
				if env.Messages[i].Role == "assistant" {
					fromAgentEnd = concatText(env.Messages[i].Content)
					break
				}
			}
		case "turn_end":
			if env.Message != nil && env.Message.Role == "assistant" {
				fromTurnEnd = concatText(env.Message.Content)
			}
		case "message_end":
			if env.Message != nil && env.Message.Role == "assistant" {
				fromMessageEnd = concatText(env.Message.Content)
			}
		}
	}

	if !anyEventObserved {
		return "", fmt.Errorf("pi stdout contained no JSON events")
	}

	switch {
	case fromAgentEnd != "":
		return fromAgentEnd, nil
	case fromTurnEnd != "":
		return fromTurnEnd, nil
	case fromMessageEnd != "":
		return fromMessageEnd, nil
	default:
		return "", fmt.Errorf("pi stdout contained no assistant message text")
	}
}

func concatText(blocks []piContentBlock) string {
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// stripCodeFences removes a single surrounding ```json ... ``` or ``` ... ```
// fence if present. Pi's system prompt forbids fences, but some model
// completions still emit them — be defensive.
func stripCodeFences(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Drop the leading fence (and optional language tag).
	first := strings.IndexByte(s, '\n')
	if first == -1 {
		return s
	}
	s = s[first+1:]
	// Drop the trailing fence.
	if i := strings.LastIndex(s, "```"); i != -1 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}
