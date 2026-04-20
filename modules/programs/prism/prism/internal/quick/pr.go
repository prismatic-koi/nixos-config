// Package quickpr implements the "prism quick pr" command.
//
// It generates a PR description from staged git changes using the OpenRouter
// API, then creates a branch, commits, pushes, and opens a GitHub PR —
// all from main. On success it switches back to main and opens the PR in
// the system browser.
package quick

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/prismatic-koi/prism/internal/config"
)

const (
	openrouterURL = "https://openrouter.ai/api/v1/chat/completions"

	// Token thresholds for max_tokens selection.
	smallLines  = 50
	mediumLines = 200

	smallTokens  = 128
	mediumTokens = 256
	largeTokens  = 512
)

// Run executes the quick pr workflow.
func Run() error {
	// ── Pre-flight checks ──────────────────────────────────────────────────

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

	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("OPENROUTER_API_KEY is not set")
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

	// ── Git diff analysis ──────────────────────────────────────────────────

	diff, err := stagedDiff()
	if err != nil {
		return err
	}

	lineCount := countDiffLines(diff)
	maxTokens := tokenBudget(lineCount)

	// ── Generate PR description via OpenRouter ─────────────────────────────

	description, err := generateDescription(apiKey, qp, diff, maxTokens)
	if err != nil {
		return err
	}

	// Extract title (first line) and body (rest).
	title, body := splitDescription(description)

	// ── Git operations ─────────────────────────────────────────────────────

	newBranch := fmt.Sprintf("quick/pr-%d", time.Now().Unix())
	fmt.Printf("Creating branch %s...\n", newBranch)

	if err := gitRun("switch", "-c", newBranch); err != nil {
		return fmt.Errorf("git switch -c: %w", err)
	}

	// Ensure we switch back to main even if something fails after this point.
	defer func() {
		_ = gitRun("switch", "main")
	}()

	if err := gitRun("commit", "-m", title); err != nil {
		return fmt.Errorf("git commit: %w", err)
	}

	fmt.Println("Pushing branch...")
	if err := gitRun("push", "-u", "origin", newBranch); err != nil {
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
	if err := ghRun(prArgs...); err != nil {
		return fmt.Errorf("gh pr create: %w", err)
	}

	// ── Fetch PR URL ───────────────────────────────────────────────────────

	prURL, err := ghOutput("pr", "view", "--json", "url", "-q", ".url")
	if err != nil {
		return fmt.Errorf("gh pr view: %w", err)
	}
	prURL = strings.TrimSpace(prURL)

	fmt.Printf("PR created: %s\n", prURL)

	// ── Switch back to main ────────────────────────────────────────────────
	// The deferred switch handles the actual switch; we cancel it here so we
	// can print a clean message, then let the defer run anyway (no-op on main).
	if err := gitRun("switch", "main"); err != nil {
		return fmt.Errorf("git switch main: %w", err)
	}

	// ── Open browser ───────────────────────────────────────────────────────

	if prURL != "" {
		if err := openBrowser(prURL); err != nil {
			// Non-fatal: just print the URL so the user can open it manually.
			fmt.Fprintf(os.Stderr, "warning: could not open browser: %v\n", err)
		}
	}

	return nil
}

// ── Helpers ────────────────────────────────────────────────────────────────

func currentBranch() (string, error) {
	out, err := gitOutput("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %w", err)
	}
	return strings.TrimSpace(out), nil
}

func stagedFiles() ([]string, error) {
	out, err := gitOutput("diff", "--cached", "--name-only")
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
	out, err := gitOutput("diff", "--cached")
	if err != nil {
		return "", fmt.Errorf("git diff --cached: %w", err)
	}
	return out, nil
}

// countDiffLines counts the number of added + removed lines in a unified diff.
func countDiffLines(diff string) int {
	count := 0
	for _, line := range strings.Split(diff, "\n") {
		if len(line) == 0 {
			continue
		}
		if line[0] == '+' || line[0] == '-' {
			// Skip the +++ / --- header lines.
			if strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") {
				continue
			}
			count++
		}
	}
	return count
}

// tokenBudget returns the max_tokens value based on the number of diff lines.
func tokenBudget(lines int) int {
	switch {
	case lines <= smallLines:
		return smallTokens
	case lines <= mediumLines:
		return mediumTokens
	default:
		return largeTokens
	}
}

// splitDescription splits the generated text into a title (first line) and
// body (remaining lines). The title is truncated to 72 characters.
func splitDescription(desc string) (title, body string) {
	desc = strings.TrimSpace(desc)
	idx := strings.Index(desc, "\n")
	if idx == -1 {
		title = desc
		body = ""
	} else {
		title = strings.TrimSpace(desc[:idx])
		body = strings.TrimSpace(desc[idx+1:])
	}
	if len(title) > 72 {
		title = title[:72]
	}
	return title, body
}

// openRouterRequest is the JSON body sent to OpenRouter.
type openRouterRequest struct {
	Model     string              `json:"model"`
	MaxTokens int                 `json:"max_tokens"`
	Messages  []map[string]string `json:"messages"`
	Provider  *providerConfig     `json:"provider,omitempty"`
}

type providerConfig struct {
	Order          []string `json:"order,omitempty"`
	AllowFallbacks bool     `json:"allow_fallbacks"`
}

type openRouterResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Code    int    `json:"code"`
	} `json:"error,omitempty"`
}

var prPromptTemplate = `You are a concise technical writer. Given the following git diff of staged changes, write a pull request title and description.

Format your response as:
<title> (one line, imperative mood, max 72 chars, no period)

<body> (2-4 sentences describing what changed and why, plain text, no markdown headers)

Git diff:
%s`

func generateDescription(apiKey string, qp config.QuickProfile, diff string, maxTokens int) (string, error) {
	prompt := fmt.Sprintf(prPromptTemplate, diff)

	reqBody := openRouterRequest{
		Model:     qp.Model,
		MaxTokens: maxTokens,
		Messages: []map[string]string{
			{"role": "user", "content": prompt},
		},
	}

	if len(qp.ProviderOrder) > 0 {
		reqBody.Provider = &providerConfig{
			Order:          qp.ProviderOrder,
			AllowFallbacks: true,
		}
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("quick pr: marshal request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, openrouterURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("quick pr: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("quick pr: OpenRouter request failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("quick pr: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("quick pr: OpenRouter API error (HTTP %d): %s", resp.StatusCode, string(respBytes))
	}

	var orResp openRouterResponse
	if err := json.Unmarshal(respBytes, &orResp); err != nil {
		return "", fmt.Errorf("quick pr: parse response: %w", err)
	}

	if orResp.Error != nil {
		return "", fmt.Errorf("quick pr: OpenRouter API error (code %d): %s", orResp.Error.Code, orResp.Error.Message)
	}

	if len(orResp.Choices) == 0 || orResp.Choices[0].Message.Content == "" {
		return "", fmt.Errorf("quick pr: empty response from OpenRouter")
	}

	return orResp.Choices[0].Message.Content, nil
}

func gitRun(args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func gitOutput(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func ghRun(args ...string) error {
	cmd := exec.Command("gh", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func ghOutput(args ...string) (string, error) {
	cmd := exec.Command("gh", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	default: // linux and others
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
