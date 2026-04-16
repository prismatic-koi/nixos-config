// Package opencode implements the harness.Harness interface for the opencode
// agent runtime. It encapsulates all opencode-specific logic: container command,
// health-check, config mount path, session creation, prompt delivery, SSE
// subscription, event type mapping, and message extraction.
//
// The sidecar imports this package directly and uses it via the concrete
// *Adapter type. Injection of the harness.Harness interface into the sidecar
// constructor is a separate issue (#710).
package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prismatic-koi/prism/internal/harness"
	"github.com/prismatic-koi/prism/internal/sse"
)

// containerPort is the port opencode serve listens on inside the container.
// Mirrors container.ContainerPort but avoids an import cycle.
const containerPort = 4096

// healthCheckInterval is the pause between consecutive health-check probes.
const healthCheckInterval = 500 * time.Millisecond

// defaultHealthCheckTimeout is the maximum time to wait for the container
// to become healthy. Matches container.DefaultHealthCheckTimeout.
const defaultHealthCheckTimeout = 60 * time.Second

// Compile-time assertion that *Adapter implements harness.Harness.
var _ harness.Harness = (*Adapter)(nil)

// Adapter implements harness.Harness for the opencode runtime. It holds the
// opencode HTTP base URL and the HTTP client used for all API calls.
type Adapter struct {
	opencodeURL string
	httpClient  *http.Client
	agentRole   string
	agentModel  string

	mu        sync.Mutex
	sessionID string // set by CreateSession; used by DeliverInitialPrompt
}

// New creates a new Adapter for the given opencode base URL.
//
// opencodeURL is the base URL of the opencode HTTP server (e.g.
// "http://localhost:4096"). httpClient is used for all HTTP requests; pass nil
// to use a default short-timeout client. agentRole is the agent role for this
// session (e.g. "worker" or "coordinator"); agentModel is the model identifier
// (e.g. "anthropic/claude-sonnet-4-6").
func New(opencodeURL string, httpClient *http.Client, agentRole, agentModel string) *Adapter {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &Adapter{
		opencodeURL: opencodeURL,
		httpClient:  httpClient,
		agentRole:   agentRole,
		agentModel:  agentModel,
	}
}

// ContainerCommand returns the command string used to launch opencode as the
// main process inside its container.
func (a *Adapter) ContainerCommand() string {
	return fmt.Sprintf("opencode serve --port %d --hostname 0.0.0.0", containerPort)
}

// HealthCheck probes GET /global/health on the given port until it responds
// with 2xx or ctx is cancelled. Returns nil when healthy.
//
// /global/health is used rather than / because the root URL falls through to
// opencode's UIRoutes catch-all, which proxies to https://app.opencode.ai/
// when there is no embedded web UI — adding a 3–4 s network round-trip on
// every container startup. /global/health is in ControlPlaneRoutes and returns
// immediately with no external I/O.
func (a *Adapter) HealthCheck(ctx context.Context, port int) error {
	healthURL := fmt.Sprintf("http://127.0.0.1:%d/global/health", port)
	client := a.httpClient

	deadline := time.Now().Add(defaultHealthCheckTimeout)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("opencode: health check timed out after %s on port %d", defaultHealthCheckTimeout, port)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(healthCheckInterval):
		}
	}
}

// ConfigMountPath returns the XDG config path inside the container where
// opencode expects its configuration to be mounted.
func (a *Adapter) ConfigMountPath() string {
	return "/root/.config/opencode"
}

// CreateSession creates a new session on the opencode server via POST /session
// and returns its ID. The session ID is also stored in the adapter so that
// DeliverInitialPrompt can use it without the caller having to pass it back.
//
// This method is not part of the harness.Harness interface. It is called by
// the sidecar directly (before the interface-injection migration in #710) so
// that the sidecar can write the session ID file between creation and prompt
// delivery — the ordering matters for the opencode attach TUI flow.
func (a *Adapter) CreateSession(ctx context.Context) (string, error) {
	body := map[string]string{"directory": "/workspace"}
	jsonBytes, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("opencode: marshal session body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.opencodeURL+"/session", bytes.NewReader(jsonBytes))
	if err != nil {
		return "", fmt.Errorf("opencode: create session request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("opencode: create session http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("opencode: create session: http status %d", resp.StatusCode)
	}

	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("opencode: decode session response: %w", err)
	}
	if result.ID == "" {
		return "", fmt.Errorf("opencode: empty session ID in response")
	}

	a.mu.Lock()
	a.sessionID = result.ID
	a.mu.Unlock()

	return result.ID, nil
}

// SessionID returns the most recently created session ID (set by CreateSession).
// Returns an empty string if CreateSession has not been called or failed.
func (a *Adapter) SessionID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessionID
}

// DeliverInitialPrompt delivers the initial prompt to the session previously
// created by CreateSession. The role parameter overrides the adapter's
// configured agentRole when non-empty.
//
// It satisfies the harness.Harness interface. When called via the interface
// (post-#710), the adapter will need to have already had CreateSession called
// (or will create a session internally). For the current direct-call path,
// CreateSession is always called first by the sidecar.
func (a *Adapter) DeliverInitialPrompt(ctx context.Context, prompt, role string) error {
	a.mu.Lock()
	sid := a.sessionID
	a.mu.Unlock()

	if sid == "" {
		return fmt.Errorf("opencode: DeliverInitialPrompt called without a session ID — call CreateSession first")
	}

	agentRole := role
	if agentRole == "" {
		agentRole = a.agentRole
	}
	if agentRole == "" {
		agentRole = "worker"
	}

	body := map[string]any{
		"parts": []map[string]string{
			{"type": "text", "text": prompt},
		},
		"agent": agentRole,
	}

	if a.agentModel != "" {
		slashIdx := strings.Index(a.agentModel, "/")
		if slashIdx >= 0 {
			body["model"] = map[string]string{
				"providerID": a.agentModel[:slashIdx],
				"modelID":    a.agentModel[slashIdx+1:],
			}
		}
	}

	jsonBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("opencode: marshal prompt body: %w", err)
	}

	url := fmt.Sprintf("%s/session/%s/prompt_async", a.opencodeURL, sid)
	log.Printf("opencode: DeliverInitialPrompt: POST %s (agent=%s)", url, agentRole)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(jsonBytes))
	if err != nil {
		return fmt.Errorf("opencode: create prompt request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("opencode: deliver prompt http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("opencode: deliver prompt: http status %d", resp.StatusCode)
	}

	log.Printf("opencode: DeliverInitialPrompt: completed for session %s", sid)
	return nil
}

// DeliverPrompt delivers a follow-up prompt to the current opencode session.
// It satisfies the harness.Harness interface. The session ID must have been
// established via CreateSession or a prior DeliverInitialPrompt call.
func (a *Adapter) DeliverPrompt(ctx context.Context, prompt string) error {
	return a.DeliverInitialPrompt(ctx, prompt, "")
}

// Subscribe connects to the opencode SSE event stream and returns a channel of
// harness.HarnessEvents. The channel is closed when the stream ends or ctx is
// cancelled. It uses the same retry/reconnect policy as the previous inline
// sse.Client setup in the sidecar.
func (a *Adapter) Subscribe(ctx context.Context) (<-chan harness.HarnessEvent, error) {
	url := a.opencodeURL + "/event"
	client := &sse.Client{
		InitialRetryDelay: 1 * time.Second,
		MaxRetryDelay:     30 * time.Second,
	}

	sseCh, err := client.Connect(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("opencode: connect to SSE stream: %w", err)
	}

	out := make(chan harness.HarnessEvent, 64)
	go func() {
		defer close(out)
		for evt := range sseCh {
			select {
			case out <- harness.HarnessEvent{
				Type: evt.Type,
				Data: []byte(evt.Data),
			}:
			case <-ctx.Done():
				return
			}
		}
	}()

	return out, nil
}

// ExtractEventType extracts the real opencode event type from a HarnessEvent.
//
// opencode sends all events as plain `data:` lines with no `event:` field. The
// SSE spec defaults the event type to "message" when no `event:` line is
// present. The real event type is embedded in the JSON `type` field of the
// data payload.
//
// This method is not on the harness.Harness interface; it is called directly
// by the sidecar's HandleEvent to resolve the opencode-specific event type.
func (a *Adapter) ExtractEventType(evt harness.HarnessEvent) string {
	eventType := evt.Type
	if eventType == "" || eventType == "message" {
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(evt.Data, &envelope); err == nil && envelope.Type != "" {
			eventType = envelope.Type
		}
	}
	return eventType
}

// MapEvent maps an opencode HarnessEvent to a StateTransition. It returns
// (StateTransition, true) when the event represents a state change, and
// (StateTransition{}, false) when it does not.
//
// This implements the harness.Harness interface. The full opencode event
// handling logic (including timer management, DB writes, and compaction state)
// remains in the sidecar; MapEvent covers the subset of events that directly
// and unconditionally indicate a state transition.
func (a *Adapter) MapEvent(evt harness.HarnessEvent) (harness.StateTransition, bool) {
	eventType := a.ExtractEventType(evt)

	switch eventType {
	case "session.created", "session.updated":
		return harness.StateTransition{State: "active"}, true
	case "session.deleted":
		return harness.StateTransition{State: "deleted"}, true
	case "session.error":
		return harness.StateTransition{State: "error"}, true
	case "permission.asked", "question.asked":
		return harness.StateTransition{State: "waiting"}, true
	case "permission.replied", "question.replied", "question.rejected":
		return harness.StateTransition{State: "active"}, true
	}
	return harness.StateTransition{}, false
}

// ExtractMessage extracts a harness.Message from an opencode HarnessEvent.
// It returns (Message, true) for completed assistant messages and user
// messages that have accumulated text. For all other events it returns
// (Message{}, false).
//
// This is a simplified extraction for interface compliance. The sidecar's
// full message handling (including TTFT, token counts, and per-message dedup)
// is performed via HandleEvent and remains in the sidecar.
func (a *Adapter) ExtractMessage(evt harness.HarnessEvent) (harness.Message, bool) {
	eventType := a.ExtractEventType(evt)
	if eventType != "message.updated" {
		return harness.Message{}, false
	}

	var payload struct {
		Properties struct {
			Info struct {
				ID   string `json:"id"`
				Role string `json:"role"`
				Time *struct {
					Completed *float64 `json:"completed"`
				} `json:"time"`
			} `json:"info"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(evt.Data, &payload); err != nil {
		return harness.Message{}, false
	}

	info := payload.Properties.Info
	if info.ID == "" || info.Role == "" {
		return harness.Message{}, false
	}

	// For assistant messages: only emit when the message is complete.
	if info.Role == "assistant" && (info.Time == nil || info.Time.Completed == nil) {
		return harness.Message{}, false
	}

	return harness.Message{
		ID:   info.ID,
		Role: info.Role,
	}, true
}
