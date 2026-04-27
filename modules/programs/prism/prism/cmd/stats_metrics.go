package cmd

import (
	"encoding/json"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/payload"
)

// ---------- legacy per-session detail (kept for --doomloops/denials/asks) ----------

// sessionMetrics holds aggregated metrics for a single opencode session.
type sessionMetrics struct {
	SessionName      string
	HarnessSessionID string // empty string = legacy sentinel (NULL harness_session_id)
	AgentName        string
	ModelID          string
	State            string

	// Duration from first to last event.
	FirstEvent time.Time
	LastEvent  time.Time

	// Token totals.
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int

	// EventCost is the sum of per-turn costs reported directly in the opencode
	// SSE event payload (MsgAssistant.Cost). It is used as a fallback when the
	// model is not present in the local modelCosts pricing table — for example,
	// openrouter/* models that are billed at rates not known to this client.
	EventCost float64

	// Turn metrics.
	AssistantTurns int
	UserTurns      int
	TurnDurations  []time.Duration // assistant turn durations
	PeakContextPct float64

	// Tool metrics.
	ToolCalls     map[string]int             // tool name → count
	ToolDurations map[string][]time.Duration // tool name → durations

	// Compaction count.
	Compactions int

	// Subagent invocations.
	SubagentInvocations []subagentInvocation
}

// subagentInvocation records a single subagent lifecycle.
type subagentInvocation struct {
	Agent    string
	Duration time.Duration
}

func (m *sessionMetrics) totalCost() float64 {
	model := m.ModelID
	costs, ok := modelCosts[model]
	if !ok {
		// Model not in local pricing table — fall back to the cost reported
		// directly in the event payload (e.g. openrouter/* models).
		return m.EventCost
	}
	return (float64(m.InputTokens)*costs.Input +
		float64(m.OutputTokens)*costs.Output +
		float64(m.CacheReadTokens)*costs.CacheRead +
		float64(m.CacheWriteTokens)*costs.CacheWrite) / 1_000_000
}

func (m *sessionMetrics) duration() time.Duration {
	if m.FirstEvent.IsZero() || m.LastEvent.IsZero() {
		return 0
	}
	return m.LastEvent.Sub(m.FirstEvent)
}

func (m *sessionMetrics) avgTurnDuration() time.Duration {
	if len(m.TurnDurations) == 0 {
		return 0
	}
	var total time.Duration
	for _, d := range m.TurnDurations {
		total += d
	}
	return total / time.Duration(len(m.TurnDurations))
}

func (m *sessionMetrics) longestTurnDuration() time.Duration {
	var longest time.Duration
	for _, d := range m.TurnDurations {
		if d > longest {
			longest = d
		}
	}
	return longest
}

func (m *sessionMetrics) totalToolCalls() int {
	n := 0
	for _, c := range m.ToolCalls {
		n += c
	}
	return n
}

// isLegacy reports whether this sessionMetrics represents the legacy NULL-sid
// sentinel group.
func (m *sessionMetrics) isLegacy() bool {
	return m.HarnessSessionID == legacySentinel
}

// collectMetrics builds a sessionMetrics from a slice of events that all belong
// to the same harness session (or the legacy sentinel group). The harnessSessionID
// parameter is the key used for this group (legacySentinel for NULL-sid events).
//
// The model field is set to the coordinator/root agent's model if one is present
// (agent field == "coordinator"), falling back to the most-frequently-seen model.
// This gives a more meaningful "session model" than first-seen.
func collectMetrics(events []db.Event, harnessSessionID string) *sessionMetrics {
	m := &sessionMetrics{
		HarnessSessionID: harnessSessionID,
		ToolCalls:        make(map[string]int),
		ToolDurations:    make(map[string][]time.Duration),
	}

	// Track model turn counts and coordinator model for model selection.
	modelTurnCounts := make(map[string]int)
	var coordinatorModel string

	for _, e := range events {
		if m.SessionName == "" {
			m.SessionName = e.SessionName
		}
		if m.FirstEvent.IsZero() || e.CreatedAt.Before(m.FirstEvent) {
			m.FirstEvent = e.CreatedAt
		}
		if e.CreatedAt.After(m.LastEvent) {
			m.LastEvent = e.CreatedAt
		}

		switch e.Type {
		case "msg_assistant":
			var p payload.MsgAssistant
			if err := json.Unmarshal([]byte(e.Payload), &p); err == nil {
				m.AssistantTurns++
				m.InputTokens += p.InputTokens
				m.OutputTokens += p.OutputTokens
				m.CacheReadTokens += p.CacheReadTokens
				m.CacheWriteTokens += p.CacheWriteTokens
				m.EventCost += p.Cost
				if p.DurationMs > 0 {
					m.TurnDurations = append(m.TurnDurations, time.Duration(p.DurationMs)*time.Millisecond)
				}
				if p.ContextWindowPct > m.PeakContextPct {
					m.PeakContextPct = p.ContextWindowPct
				}
				if m.AgentName == "" && p.Agent != "" {
					m.AgentName = p.Agent
				}
				if p.Model != "" {
					modelTurnCounts[p.Model]++
					// Prefer coordinator model: take the first coordinator model seen.
					if p.Agent == "coordinator" && coordinatorModel == "" {
						coordinatorModel = p.Model
					}
				}
			}

		case "msg_user":
			m.UserTurns++

		case "tool_call":
			var p payload.ToolCall
			if err := json.Unmarshal([]byte(e.Payload), &p); err == nil {
				m.ToolCalls[p.Tool]++
				if p.DurationMs > 0 {
					m.ToolDurations[p.Tool] = append(m.ToolDurations[p.Tool], time.Duration(p.DurationMs)*time.Millisecond)
				}
			}

		case "compaction":
			m.Compactions++

		case "subagent_end":
			var p payload.SubagentEnd
			if err := json.Unmarshal([]byte(e.Payload), &p); err == nil {
				dur := time.Duration(0)
				if p.DurationMs > 0 {
					dur = time.Duration(p.DurationMs) * time.Millisecond
				}
				m.SubagentInvocations = append(m.SubagentInvocations, subagentInvocation{
					Agent:    p.Agent,
					Duration: dur,
				})
			}
		}
	}

	// Determine the best model: coordinator model takes priority, then
	// fall back to the most-frequently-seen model.
	if coordinatorModel != "" {
		m.ModelID = coordinatorModel
	} else {
		// Pick most frequent model.
		var bestModel string
		var bestCount int
		for model, count := range modelTurnCounts {
			if count > bestCount || (count == bestCount && model < bestModel) {
				bestModel = model
				bestCount = count
			}
		}
		m.ModelID = bestModel
	}

	return m
}

// groupEventsByHarnessSessionID partitions events by harness_session_id, preserving
// insertion order for the first occurrence of each key.
// Events with a NULL harness_session_id are grouped under legacySentinel ("").
// Returns the grouped map and the ordered list of keys.
func groupEventsByHarnessSessionID(events []db.Event) (map[string][]db.Event, []string) {
	grouped := make(map[string][]db.Event)
	var order []string

	for _, e := range events {
		key := legacySentinel
		if e.HarnessSessionID != nil {
			key = *e.HarnessSessionID
		}
		if _, exists := grouped[key]; !exists {
			order = append(order, key)
		}
		grouped[key] = append(grouped[key], e)
	}

	return grouped, order
}

// computeTurnCost computes the cost for a single msg_assistant turn.
// Uses the local pricing table when the model is known; falls back to
// the event-reported cost for unknown models (e.g. openrouter/*).
func computeTurnCost(t db.TokenTurn) float64 {
	costs, ok := modelCosts[t.Model]
	if !ok {
		return t.EventCost
	}
	return (float64(t.Input)*costs.Input +
		float64(t.Output)*costs.Output +
		float64(t.CacheRead)*costs.CacheRead +
		float64(t.CacheWrite)*costs.CacheWrite) / 1_000_000
}

// modelMetrics tracks per-model metrics accumulated across turns.
type modelMetrics struct {
	Provider     string
	Model        string
	Turns        int
	Sessions     map[string]struct{} // distinct opencode session IDs
	DurationsMs  []float64           // durationMs values for P50 (zero values excluded)
	TtftMs       []float64           // ttftMs values for P50 (zero/absent values excluded)
	TokPerSec    []float64           // output tokens/sec per turn (zero-duration turns excluded)
	InputTokens  int
	OutputTokens int
	Cost         float64
	AgentCounts  map[string]int // agent name → turn count for this model
}

// collectModelMetrics groups msg_assistant events by provider/model and builds
// per-model metrics. Turns with durationMs == 0 are excluded from latency and
// throughput calculations.
//
// Sessions are counted by distinct harness_session_id (not session_name) so that
// long-lived tmux sessions with multiple opencode sessions are counted correctly.
// Events with a NULL harness_session_id are counted as distinct sessions only if the
// session_name is distinct — they're bucketed by session_name as a fallback.
func collectModelMetrics(events []db.Event) map[string]*modelMetrics {
	metrics := make(map[string]*modelMetrics)

	for _, e := range events {
		if e.Type != "msg_assistant" {
			continue
		}

		var p payload.MsgAssistant
		if err := json.Unmarshal([]byte(e.Payload), &p); err != nil {
			continue
		}
		if p.Model == "" {
			continue
		}

		provider, model := splitModel(p.Model)
		key := p.Model // full "provider/model" as map key
		m, ok := metrics[key]
		if !ok {
			m = &modelMetrics{
				Provider:    provider,
				Model:       model,
				Sessions:    make(map[string]struct{}),
				AgentCounts: make(map[string]int),
			}
			metrics[key] = m
		}

		m.Turns++

		// Count sessions by harness_session_id when available; fall back to session_name
		// for NULL-sid (legacy) events so they still contribute a session count.
		if e.HarnessSessionID != nil && *e.HarnessSessionID != "" {
			m.Sessions[*e.HarnessSessionID] = struct{}{}
		} else {
			m.Sessions["legacy:"+e.SessionName] = struct{}{}
		}

		m.InputTokens += p.InputTokens
		m.OutputTokens += p.OutputTokens

		// Track agent breakdown.
		if p.Agent != "" {
			m.AgentCounts[p.Agent]++
		}

		// Cost using the full key (provider/model).
		// If the model is not in the local pricing table, fall back to the
		// cost reported in the event payload (e.g. openrouter/* models).
		if costs, ok := modelCosts[key]; ok {
			m.Cost += (float64(p.InputTokens)*costs.Input +
				float64(p.OutputTokens)*costs.Output +
				float64(p.CacheReadTokens)*costs.CacheRead +
				float64(p.CacheWriteTokens)*costs.CacheWrite) / 1_000_000
		} else {
			m.Cost += p.Cost
		}

		// Latency and throughput only for turns with valid duration.
		if p.DurationMs > 0 {
			m.DurationsMs = append(m.DurationsMs, float64(p.DurationMs))
			secs := float64(p.DurationMs) / 1000.0
			tokPerSec := float64(p.OutputTokens) / secs
			m.TokPerSec = append(m.TokPerSec, tokPerSec)
		}

		// TTFT only for turns with a non-zero ttftMs value.
		if p.TtftMs > 0 {
			m.TtftMs = append(m.TtftMs, float64(p.TtftMs))
		}
	}

	return metrics
}
