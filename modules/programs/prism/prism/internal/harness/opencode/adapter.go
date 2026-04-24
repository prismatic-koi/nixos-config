// Package opencode implements the harness.Harness interface for the opencode
// agent runtime. It encapsulates all opencode-specific logic: container command,
// health-check, config mount path, session creation, prompt delivery, SSE
// subscription, event type mapping, and message extraction.
//
// This is the reference implementation of harness.Harness. The sidecar
// receives an *Adapter (as a harness.Harness interface value) at construction
// time via sidecar.Config.Harness — injected by cmd/sidecar.go (#710).
package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/harness"
	"github.com/prismatic-koi/prism/internal/sse"
)

// containerPort is the port opencode listens on inside the container.
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
	// containerMode, when true, causes DeliverInitialPrompt to be a no-op.
	// In container mode the initial prompt is delivered via --prompt on the
	// opencode command line (RFC #691 Phase 1a), so HTTP delivery would be
	// redundant and would target the wrong session.
	containerMode bool

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
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}
	return &Adapter{
		opencodeURL: opencodeURL,
		httpClient:  httpClient,
		agentRole:   agentRole,
		agentModel:  agentModel,
	}
}

// NewContainerMode creates a new Adapter configured for container mode.
// In this mode, DeliverInitialPrompt is a no-op because the prompt is
// delivered via --prompt on the opencode CLI at container startup.
// CreateSession uses GET /session to retrieve the existing session ID
// rather than POST /session which would create a second session.
func NewContainerMode(opencodeURL string, httpClient *http.Client, agentRole, agentModel string) *Adapter {
	a := New(opencodeURL, httpClient, agentRole, agentModel)
	a.containerMode = true
	return a
}

// ContainerCommand returns the base command string used to launch opencode as
// the main process inside its container. "opencode --port N --hostname 0.0.0.0"
// launches opencode in combined TUI + HTTP mode: the TUI renders on the
// container's PTY (bridged to the tmux pane via "podman attach") while the
// HTTP/SSE API is served on containerPort for the sidecar (RFC #691, Phase 1a).
//
// Note: the actual container launch in buildRunArgs() appends --agent and
// --prompt directly when InitialPrompt is set — the CLI flag approach ensures
// the conversation is visible in the TUI from the start (rather than being
// sent to a second session created by POST /session).
func (a *Adapter) ContainerCommand() string {
	return fmt.Sprintf("opencode --port %d --hostname 0.0.0.0", containerPort)
}

// healthProbeClient is a short-timeout HTTP client used exclusively for
// individual health-check probe attempts. Its timeout governs a single HTTP
// round-trip, not the overall health-check deadline (which is controlled by
// defaultHealthCheckTimeout and enforced in the HealthCheck loop). Using a
// separate client avoids reusing the general-purpose 20 s API client for probe
// attempts that must complete within one healthCheckInterval window.
var healthProbeClient = &http.Client{Timeout: defaultHealthCheckTimeout}

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
		resp, err := healthProbeClient.Do(req)
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

// sessionEntry is a single entry from the opencode GET /session response.
type sessionEntry struct {
	ID   string `json:"id"`
	Time *struct {
		Updated *float64 `json:"updated"`
	} `json:"time"`
}

// createSessionPerAttemptTimeout is the HTTP client timeout for each individual
// GET /session attempt. This is intentionally short so that a hung opencode
// process (one that has bound the HTTP port but not yet initialised its session
// layer) times out quickly and the retry loop can try again rather than burning
// the entire available window on a single unresponsive attempt.
const createSessionPerAttemptTimeout = 5 * time.Second

// createSessionMaxAttempts is the maximum number of GET /session attempts
// before CreateSession gives up. With createSessionPerAttemptTimeout and
// createSessionRetryInterval, the effective ceiling is approximately
// maxAttempts*(perAttemptTimeout+retryInterval) ≈ 44s.
const createSessionMaxAttempts = 8

// createSessionRetryInterval is the fixed pause between consecutive
// GET /session attempts on transport-level failures.
const createSessionRetryInterval = 500 * time.Millisecond

// CreateSession retrieves the existing opencode session via GET /session and
// returns its ID. The session ID is also stored in the adapter so that
// subsequent prism prompt delivery (via DeliverPrompt / the host-API relay)
// can target the correct session.
//
// In combined TUI + HTTP mode with --prompt, opencode creates a session and
// starts processing the prompt immediately. The sidecar calls CreateSession
// after the health check to discover that session ID — the ID is needed for
// subsequent prism prompt follow-up delivery even though the initial prompt
// was already sent via CLI. We use GET /session rather than POST /session
// because POST would create a redundant second session.
//
// GET /session is retried up to createSessionMaxAttempts times with a
// createSessionPerAttemptTimeout per-attempt timeout to survive opencode's
// two-phase startup: /global/health responds as soon as the HTTP port is bound,
// but /session is only ready after opencode has finished its full initialisation
// sequence (plugin loading, MCP proxy init, session creation). Under CPU
// contention a single long request can time out at the transport level before
// opencode is ready; short per-attempt timeouts with retries ride out the lag.
//
// If GET /session returns an empty list (opencode hasn't finished creating its
// initial session yet), this method retries for up to 5s with a shorter poll
// interval so the caller is not delayed on the fast path.
//
// It satisfies the harness.Harness interface.
func (a *Adapter) CreateSession(ctx context.Context) (string, error) {
	url := a.opencodeURL + "/session"
	sessionStart := time.Now()

	// perAttemptClient uses a short timeout so that a hung opencode process
	// (port bound but not yet session-ready) fails quickly, allowing the retry
	// loop to make another attempt rather than blocking for the full 20s shared
	// client timeout.
	perAttemptClient := &http.Client{Timeout: createSessionPerAttemptTimeout}

	var lastErr error
	for attempt := 1; attempt <= createSessionMaxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return "", fmt.Errorf("opencode: GET /session request: %w", err)
		}

		resp, err := perAttemptClient.Do(req)
		if err != nil {
			lastErr = err
			log.Printf("sidecar: GET /session attempt %d/%d: %v", attempt, createSessionMaxAttempts, err)
			if attempt < createSessionMaxAttempts {
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(createSessionRetryInterval):
				}
			}
			continue
		}

		var sessions []sessionEntry
		decodeErr := json.NewDecoder(resp.Body).Decode(&sessions)
		_ = resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", fmt.Errorf("opencode: GET /session: http status %d", resp.StatusCode)
		}
		if decodeErr != nil {
			return "", fmt.Errorf("opencode: decode GET /session response: %w", decodeErr)
		}

		if len(sessions) > 0 {
			// Pick the most recently updated session (opencode may surface
			// multiple sessions if the user was previously active in this
			// workspace; we want the one that just started).
			best := sessions[0]
			for _, s := range sessions[1:] {
				if s.Time != nil && s.Time.Updated != nil {
					if best.Time == nil || best.Time.Updated == nil || *s.Time.Updated > *best.Time.Updated {
						best = s
					}
				}
			}
			elapsed := time.Since(sessionStart).Round(time.Millisecond)
			log.Printf("opencode: CreateSession: retrieved existing session %q from GET /session (%d session(s)) [%s after CreateSession start]", best.ID, len(sessions), elapsed)

			a.mu.Lock()
			a.sessionID = best.ID
			a.mu.Unlock()

			return best.ID, nil
		}

		// Empty list — opencode has responded but hasn't created its initial
		// session yet (a narrow window between health-check passing and the
		// first session being written). Poll with a short interval for up to
		// 5s before giving up.
		const emptyListMaxWait = 5 * time.Second
		const emptyListInterval = 200 * time.Millisecond
		emptyDeadline := time.Now().Add(emptyListMaxWait)
		log.Printf("opencode: CreateSession: GET /session returned empty list — retrying in %s", emptyListInterval)
		for {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(emptyListInterval):
			}

			req2, err2 := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err2 != nil {
				return "", fmt.Errorf("opencode: GET /session request: %w", err2)
			}
			resp2, err2 := perAttemptClient.Do(req2)
			if err2 != nil {
				// Transport error during empty-list poll — treat as a fresh
				// transport failure and let the outer retry loop handle it.
				lastErr = err2
				log.Printf("sidecar: GET /session attempt %d/%d: %v", attempt, createSessionMaxAttempts, err2)
				break
			}

			var sessions2 []sessionEntry
			decodeErr2 := json.NewDecoder(resp2.Body).Decode(&sessions2)
			_ = resp2.Body.Close()

			if resp2.StatusCode < 200 || resp2.StatusCode >= 300 {
				return "", fmt.Errorf("opencode: GET /session: http status %d", resp2.StatusCode)
			}
			if decodeErr2 != nil {
				return "", fmt.Errorf("opencode: decode GET /session response: %w", decodeErr2)
			}

			if len(sessions2) > 0 {
				best := sessions2[0]
				for _, s := range sessions2[1:] {
					if s.Time != nil && s.Time.Updated != nil {
						if best.Time == nil || best.Time.Updated == nil || *s.Time.Updated > *best.Time.Updated {
							best = s
						}
					}
				}
				elapsed := time.Since(sessionStart).Round(time.Millisecond)
				log.Printf("opencode: CreateSession: retrieved existing session %q from GET /session (%d session(s)) [%s after CreateSession start]", best.ID, len(sessions2), elapsed)

				a.mu.Lock()
				a.sessionID = best.ID
				a.mu.Unlock()

				return best.ID, nil
			}

			if time.Now().After(emptyDeadline) {
				return "", fmt.Errorf("opencode: GET /session: no sessions available after %s", emptyListMaxWait)
			}
			log.Printf("opencode: CreateSession: GET /session returned empty list — retrying in %s", emptyListInterval)
		}

		// Transport error broke out of the empty-list loop; sleep before next attempt.
		if attempt < createSessionMaxAttempts {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(createSessionRetryInterval):
			}
		}
	}

	log.Printf("sidecar: GET /session failed after %d attempts", createSessionMaxAttempts)
	if lastErr != nil {
		return "", fmt.Errorf("opencode: GET /session failed after %d attempts: %w", createSessionMaxAttempts, lastErr)
	}
	return "", fmt.Errorf("opencode: GET /session failed after %d attempts", createSessionMaxAttempts)
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
// In container mode (containerMode=true), this is a no-op: the prompt was
// already delivered via --prompt on the opencode CLI at container startup
// (RFC #691 Phase 1a). HTTP delivery here would send to the wrong session.
//
// It satisfies the harness.Harness interface.
func (a *Adapter) DeliverInitialPrompt(ctx context.Context, prompt, role string) error {
	if a.containerMode {
		log.Printf("opencode: DeliverInitialPrompt: container mode — prompt already delivered via CLI, skipping HTTP delivery")
		return nil
	}

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
// It satisfies the harness.Harness interface. The sidecar calls this via the
// interface in HandleEvent to resolve the opencode-specific event type before
// dispatching to the appropriate event handler.
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
		return harness.StateTransition{State: agent.StateActive}, true
	case "session.deleted":
		return harness.StateTransition{State: agent.StateDeleted}, true
	case "session.error":
		return harness.StateTransition{State: agent.StateError}, true
	case "permission.asked", "question.asked":
		return harness.StateTransition{State: agent.StateWaiting}, true
	case "permission.replied", "question.replied", "question.rejected":
		return harness.StateTransition{State: agent.StateActive}, true
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

// configEnvVarName is the environment variable opencode uses to receive
// serialised config content (model, variant, and provider overrides).
const configEnvVarName = "OPENCODE_CONFIG_CONTENT"

// bashTimeoutEnvVar is the env var that controls opencode's experimental
// bash-tool execution timeout (in milliseconds).
const bashTimeoutEnvVar = "OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS"

// bashTimeoutValue is the default bash-tool timeout value (15 minutes).
// This raises the 2-minute default so that long-running commands
// (e.g. prism review, nix build) are not killed mid-flight.
const bashTimeoutValue = "900000"

// ConfigEnvVar returns "OPENCODE_CONFIG_CONTENT" — the environment variable
// opencode uses to receive its serialised config content.
// It satisfies the harness.Harness interface.
func (a *Adapter) ConfigEnvVar() string {
	return configEnvVarName
}

// RuntimeEnv returns the additional environment variables opencode needs
// in the container / process it runs in. Currently this is just the
// experimental bash-tool timeout raised to 15 minutes.
// It satisfies the harness.Harness interface.
func (a *Adapter) RuntimeEnv() map[string]string {
	return map[string]string{
		bashTimeoutEnvVar: bashTimeoutValue,
	}
}

// ValidateAgentRole reports whether the given agent role is supported by
// opencode. It checks that the agent definition file (<role>.md) exists in
// the opencode agents directory ($XDG_CONFIG_HOME/opencode/agents/).
//
// Returns nil when the role is valid. Returns a descriptive error
// mentioning the harness name ("opencode") and the role when the agent
// definition is missing.
//
// It satisfies the harness.Harness interface.
func (a *Adapter) ValidateAgentRole(role string) error {
	dir := agentsDir()
	path := filepath.Join(dir, role+".md")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return fmt.Errorf(
			"opencode: agent role %q is not available — no definition file at %s\n"+
				"hint: ensure the system has been rebuilt with the prism NixOS module",
			role, path,
		)
	}
	return nil
}

// agentsDir returns the path to the opencode agents directory.
func agentsDir() string {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, _ := os.UserHomeDir()
		configHome = filepath.Join(home, ".config")
	}
	return filepath.Join(configHome, "opencode", "agents")
}

// EffectiveModel returns the model identifier configured for the given
// agent role in opencode's config file (~/.config/opencode/opencode.json).
// Returns an empty string if the config cannot be read or the role has no
// explicit model override.
//
// It satisfies the harness.Harness interface.
func (a *Adapter) EffectiveModel(role string) string {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		configHome = filepath.Join(home, ".config")
	}
	data, err := os.ReadFile(filepath.Join(configHome, "opencode", "opencode.json"))
	if err != nil {
		return ""
	}

	// Minimal parse — just enough to extract agent.<role>.model.
	var cfg struct {
		Agent map[string]struct {
			Model string `json:"model"`
		} `json:"agent"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return ""
	}
	if ag, ok := cfg.Agent[role]; ok {
		return ag.Model
	}
	return ""
}
