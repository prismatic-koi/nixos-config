package cmd

// prism prompt — send a follow-up prompt to a running or finished agent session.
//
// Usage:
//
//	prism prompt <session> --prompt <text>
//	prism prompt <session> --prompt - < /tmp/prompt.txt
//	prism prompt <session> --prompt-file /tmp/prompt.txt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
)

// httpClient is the HTTP client used for prompt delivery. Package-level so
// tests can replace it with an httptest.Server-backed client.
var httpClient = &http.Client{Timeout: 10 * time.Second}

var promptCmd = &cobra.Command{
	Use:   "prompt <session>",
	Short: "Send a follow-up prompt to a running agent session",
	Long: `Send a follow-up message to the opencode agent running in the named tmux
session. The session must already exist and have an agent window.

The prompt is delivered directly via POST /session/:id/prompt_async for
instant delivery. A record is also written to bus_messages (with delivered_at
set) for audit. If HTTP delivery fails, an error is returned.`,
	Args: cobra.ExactArgs(1),
	RunE: runPrompt,
}

func init() {
	addPromptFlags(promptCmd)
	rootCmd.AddCommand(promptCmd)
}

func runPrompt(cmd *cobra.Command, args []string) error {
	sessionName := args[0]

	promptText, err := requirePromptInput(cmd)
	if err != nil {
		return err
	}

	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
		return proxyPrompt(apiURL, sessionName, promptText)
	}

	// Open DB.
	database, err := openDB()
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer database.Close()

	// Check target session status.
	status, err := database.CurrentStatus(sessionName)
	if err != nil {
		return fmt.Errorf("check session status: %w", err)
	}

	if status == nil {
		return fmt.Errorf(
			"session %q not found in DB\nrun `prism list-sessions` to see available sessions",
			sessionName,
		)
	}

	if status.EndedAt != nil {
		return fmt.Errorf(
			"session %q has ended — escalate to user to restart if needed",
			sessionName,
		)
	}

	if status.State == "waiting" {
		return fmt.Errorf(
			"session %q is waiting for user input\n\n"+
				"The agent has paused and is expecting a direct response from the user.\n"+
				"Please switch to that session and respond there, or escalate to the user\n"+
				"so they can address it directly.\n\n"+
				"  prism checkin %s   — inspect the current state\n"+
				"  (C-f or C-w)       — switch to the session in tmux",
			sessionName, sessionName,
		)
	}

	// Derive from_session from the current process CWD using the .bare walk.
	fromSession := ""
	if cwd, err := os.Getwd(); err == nil {
		fromSession = deriveSessionNameFromCWD(cwd)
	}

	// Derive repo from the target session's agent_status, fallback to from_session.
	repo := status.Repo
	if repo == "" && fromSession != "" {
		// best-effort: extract repo component from from_session (format: "repo@branch")
		for i, c := range fromSession {
			if c == '@' {
				repo = fromSession[:i]
				break
			}
		}
	}

	msg := db.BusMessage{
		ID:           uuid.New().String(),
		FromSession:  fromSession,
		ToSession:    sessionName,
		ToInstanceID: status.InstanceID,
		Repo:         repo,
		Text:         promptText,
		Urgency:      "normal",
		SentAt:       time.Now(),
	}

	// Require port and session ID for HTTP delivery.
	if status.OpencodePort == nil || status.OpencodeSID == nil {
		return fmt.Errorf("session %q has no opencode port or session ID — cannot deliver prompt", sessionName)
	}

	httpErr := deliverViaHTTP(*status.OpencodePort, *status.OpencodeSID, promptText, status)
	if httpErr != nil {
		return fmt.Errorf("HTTP delivery failed: %w", httpErr)
	}

	// HTTP delivery succeeded — write audit trail with delivered_at set.
	if err := database.WriteBusMessageDelivered(msg); err != nil {
		// Non-fatal: HTTP delivery succeeded; audit write is best-effort.
		fmt.Fprintf(os.Stderr, "warning: could not write audit bus message: %v\n", err)
	}
	fmt.Printf("prompt delivered to %s via HTTP\n", sessionName)
	return nil
}

// deliverViaHTTP sends a prompt to the opencode HTTP API.
func deliverViaHTTP(port int, opencodeSID string, text string, status *db.Status) error {
	body := buildPromptBody(text, status)
	jsonBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal prompt body: %w", err)
	}

	url := fmt.Sprintf("http://localhost:%d/session/%s/prompt_async", port, opencodeSID)
	req, err := http.NewRequest("POST", url, bytes.NewReader(jsonBytes))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http status %d", resp.StatusCode)
	}

	return nil
}

// buildPromptBody constructs the request body for prompt_async.
// When root_agent_name and root_model_id are known, they are included
// so the session continues using its root agent/model. Falls back to
// agent_name/model_id for sessions created before the root fields migration.
func buildPromptBody(text string, status *db.Status) map[string]any {
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

// deriveSessionNameFromCWD walks up from cwd to find a .bare marker and
// derives the session name using the same logic as cmd/switch.go.
// Returns empty string if no .bare marker is found.
func deriveSessionNameFromCWD(cwd string) string {
	bareRoot := deriveBareRoot(cwd)
	if bareRoot == "" {
		return ""
	}
	return session.NameFor(cwd, bareRoot)
}
