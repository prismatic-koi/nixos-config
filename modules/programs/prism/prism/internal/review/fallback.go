package review

// fallback.go — re-run a review agent on a fallback model when the provider
// refuses the request (issue #3112).
//
// RunAsync records a FallbackPlan on MonitorOpts. While the monitor polls the
// round, fallbackRunner finds each agent whose run ended with a run_error
// that carries a refusal stop reason, and spawns the same agent again, in the
// same group, with the same prompt, on the next fallback model of its profile
// slot. The fallback session joins the round's group, so the round stays one
// cycle, and the report reads the verdict of the last session in the chain.

import (
	"fmt"
	"regexp"
	"strconv"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/harness"
	"github.com/prismatic-koi/prism/internal/proglog"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// refusalStopReasons are the provider stop reasons that start a fallback.
// Any other run_error ends the agent as before.
var refusalStopReasons = map[string]bool{
	"refusal":   true,
	"sensitive": true,
}

// IsRefusalStopReason reports whether a raw provider stop reason starts a
// fallback.
func IsRefusalStopReason(raw string) bool {
	return refusalStopReasons[raw]
}

// FallbackPlan holds what the monitor needs to spawn a fallback session
// through the same path RunAsync used for the first session. Every field
// except Agents is the same for all agents of the round.
type FallbackPlan struct {
	Repo               string
	Worktree           string
	PromptTemplateHash string
	IsolationMode      string
	PluginHostPath     string
	HarnessName        string
	PIExtensionDir     string
	ProfileName        string
	InvokerSession     string
	ModelsByRole       map[string]string
	// Agents is index-aligned with MonitorOpts.Agents.
	Agents []AgentFallbackPlan
}

// AgentFallbackPlan is the per-agent part of a FallbackPlan.
type AgentFallbackPlan struct {
	Prompt string
	// Model is the model of the first session.
	Model string
	// Fallbacks is the ordered list of models to try after a refusal. It is
	// empty when the slot sets none, or when --model-override names the role.
	Fallbacks []string
}

// FallbackAttempt records one fallback the monitor started.
type FallbackAttempt struct {
	// FromModel is the model of the session that the provider refused.
	FromModel string
	// Model is the model of the fallback session.
	Model string
	// RawStopReason is the provider stop reason that started the fallback.
	RawStopReason string
	// Session is the name of the fallback session.
	Session string
	// SpawnError is set when the fallback session did not spawn.
	SpawnError string `json:",omitempty"`
}

// buildFallbackPlan returns the per-agent plan for one round. A role that
// --model-override names gets no fallbacks.
func buildFallbackPlan(pf *config.ProfilesFile, profileName string, modelsByRole map[string]string, agents []Agent, prompts []string) []AgentFallbackPlan {
	out := make([]AgentFallbackPlan, len(agents))
	for i, ag := range agents {
		slot, _ := config.SlotForRole(pf, profileName, ag.Name)
		p := AgentFallbackPlan{Model: slot.Model}
		if i < len(prompts) {
			p.Prompt = prompts[i]
		}
		if override := modelsByRole[ag.Name]; override != "" {
			p.Model = override
		} else if len(slot.Fallbacks) > 0 {
			p.Fallbacks = append([]string(nil), slot.Fallbacks...)
		}
		out[i] = p
	}
	return out
}

// fallbackSpawnFunc spawns one fallback session. Tests replace it so that no
// tmux session or sidecar starts.
type fallbackSpawnFunc func(d *db.DB, in reviewerSpawnInput) error

// fallbackRunner tracks the fallback chain of each agent in one round.
type fallbackRunner struct {
	plan  *FallbackPlan
	spawn fallbackSpawnFunc
	// current is index-aligned with the round's agents: the session whose
	// result is the agent's result.
	current  []string
	attempts [][]FallbackAttempt
	// handled holds the sessions already checked for a refusal, so a
	// refused session starts at most one fallback.
	handled map[string]bool
}

func newFallbackRunner(plan *FallbackPlan, agentSessions []string, spawn fallbackSpawnFunc) *fallbackRunner {
	if spawn == nil {
		spawn = spawnFallbackSession
	}
	return &fallbackRunner{
		plan:     plan,
		spawn:    spawn,
		current:  append([]string(nil), agentSessions...),
		attempts: make([][]FallbackAttempt, len(agentSessions)),
		handled:  map[string]bool{},
	}
}

// step starts a fallback for each agent whose current session ended with a
// refusal and that has a fallback model left. It returns the number of
// fallback sessions that spawned.
func (fr *fallbackRunner) step(d *db.DB, groupID string, agents []Agent) int {
	if fr == nil || fr.plan == nil {
		return 0
	}
	groupData, err := d.GroupResults(groupID)
	if err != nil {
		proglog.Warnf("[prism monitor-review] warning: fallback check: GroupResults(%s): %v\n", groupID, err)
		return 0
	}
	started := 0
	for i, sess := range fr.current {
		if i >= len(agents) || i >= len(fr.plan.Agents) || fr.handled[sess] {
			continue
		}
		mr, ok := groupData[sess]
		if !ok || mr.State != "error" || mr.RunError == "" {
			continue
		}
		fr.handled[sess] = true
		if !IsRefusalStopReason(mr.RunErrorRawStopReason) {
			continue
		}
		ap := fr.plan.Agents[i]
		n := len(fr.attempts[i])
		if n >= len(ap.Fallbacks) {
			continue
		}
		attempt := FallbackAttempt{
			FromModel:     fr.modelOf(i),
			Model:         ap.Fallbacks[n],
			RawStopReason: mr.RunErrorRawStopReason,
			Session:       fallbackSessionName(fr.baseSession(i), n+1),
		}
		proglog.Infof("[prism monitor-review] %s on %s stopped with provider stop reason %q — re-running on %s as %s\n",
			agents[i].Name, attempt.FromModel, attempt.RawStopReason, attempt.Model, attempt.Session)
		if spawnErr := fr.spawn(d, fr.spawnInput(i, agents[i].Name, groupID, attempt)); spawnErr != nil {
			proglog.Warnf("[prism monitor-review] warning: fallback spawn for %s failed: %v\n", agents[i].Name, spawnErr)
			attempt.SpawnError = spawnErr.Error()
			fr.attempts[i] = append(fr.attempts[i], attempt)
			continue
		}
		fr.attempts[i] = append(fr.attempts[i], attempt)
		fr.current[i] = attempt.Session
		started++
	}
	return started
}

// baseSession returns the first session of agent i.
func (fr *fallbackRunner) baseSession(i int) string {
	return baseReviewSession(fr.current[i])
}

// modelOf returns the model of agent i's current session.
func (fr *fallbackRunner) modelOf(i int) string {
	for j := len(fr.attempts[i]) - 1; j >= 0; j-- {
		if fr.attempts[i][j].SpawnError == "" {
			return fr.attempts[i][j].Model
		}
	}
	if i < len(fr.plan.Agents) {
		return fr.plan.Agents[i].Model
	}
	return ""
}

// spawnInput builds the same reviewerSpawnInput RunAsync built for the first
// session, with only the session name and the model changed.
func (fr *fallbackRunner) spawnInput(i int, agentName, groupID string, a FallbackAttempt) reviewerSpawnInput {
	models := make(map[string]string, len(fr.plan.ModelsByRole)+1)
	for k, v := range fr.plan.ModelsByRole {
		models[k] = v
	}
	models[agentName] = a.Model
	return reviewerSpawnInput{
		AgentName:          agentName,
		AgentSession:       a.Session,
		Prompt:             fr.plan.Agents[i].Prompt,
		Repo:               fr.plan.Repo,
		Worktree:           fr.plan.Worktree,
		PromptTemplateHash: fr.plan.PromptTemplateHash,
		IsolationMode:      fr.plan.IsolationMode,
		PluginHostPath:     fr.plan.PluginHostPath,
		GroupID:            groupID,
		HarnessName:        fr.plan.HarnessName,
		ModelsByRole:       models,
		PIExtensionDir:     fr.plan.PIExtensionDir,
		ProfileName:        fr.plan.ProfileName,
		InvokerSession:     fr.plan.InvokerSession,
	}
}

// annotate stamps the model and the fallback chain on each result.
func (fr *fallbackRunner) annotate(results []AgentResult) {
	if fr == nil || fr.plan == nil {
		return
	}
	for i := range results {
		if i >= len(fr.attempts) {
			break
		}
		results[i].Model = fr.modelOf(i)
		results[i].Fallbacks = fr.attempts[i]
	}
}

// spawnFallbackSession is the production fallbackSpawnFunc. It goes through
// newReviewerSpawnOpts and session.SpawnSession, as RunAsync does, and
// cleans up the same way when the spawn fails.
func spawnFallbackSession(d *db.DB, in reviewerSpawnInput) error {
	h, err := harness.New(in.HarnessName, "", nil, "", "")
	if err != nil {
		return fmt.Errorf("unknown harness %q: %w", in.HarnessName, err)
	}
	in.HarnessHandle = h
	if spawnErr := session.SpawnSession(d, newReviewerSpawnOpts(in)); spawnErr != nil {
		session.KillSidecar(in.AgentSession)
		cleanupAgentSession(d, in.AgentSession, db.ReapCauseSpawnFailure, spawnErr.Error())
		_ = tmux.KillSession(in.AgentSession)
		return spawnErr
	}
	return nil
}

// fallbackSuffix matches the "-fallback-<k>" suffix of a fallback session.
var fallbackSuffix = regexp.MustCompile(`-fallback-([0-9]+)$`)

// fallbackSessionName returns the name of the k-th fallback session of base.
func fallbackSessionName(base string, k int) string {
	return fmt.Sprintf("%s-fallback-%d", base, k)
}

// baseReviewSession strips a "-fallback-<k>" suffix from a session name.
func baseReviewSession(name string) string {
	return fallbackSuffix.ReplaceAllString(name, "")
}

// fallbackIndex returns k for a "-fallback-<k>" session, and 0 otherwise.
func fallbackIndex(name string) int {
	m := fallbackSuffix.FindStringSubmatch(name)
	if m == nil {
		return 0
	}
	k, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return k
}

// EffectiveRoundSessions collapses each fallback chain in names to its last
// session, and keeps the order of first appearance. A round that ran
// fallbacks then has one result per agent, the same as a round that did not.
func EffectiveRoundSessions(names []string) []string {
	last := map[string]string{}
	var order []string
	for _, n := range names {
		base := baseReviewSession(n)
		cur, seen := last[base]
		if !seen {
			order = append(order, base)
			last[base] = n
			continue
		}
		if fallbackIndex(n) > fallbackIndex(cur) {
			last[base] = n
		}
	}
	out := make([]string, 0, len(order))
	for _, b := range order {
		out = append(out, last[b])
	}
	return out
}

// RefusalAwaitingFallback reports whether the last session of some agent in
// the group ended with a refusal run_error. The monitor can still start a
// fallback for that agent, so a group that is complete now can become
// incomplete again. members are the group's agent_status rows, and results
// the db.GroupResults map.
func RefusalAwaitingFallback(members []db.Status, results map[string]db.GroupMemberResult) bool {
	names := make([]string, 0, len(members))
	for _, m := range members {
		names = append(names, m.SessionName)
	}
	for _, sess := range EffectiveRoundSessions(names) {
		mr, ok := results[sess]
		if ok && mr.State == "error" && mr.RunError != "" && IsRefusalStopReason(mr.RunErrorRawStopReason) {
			return true
		}
	}
	return false
}
