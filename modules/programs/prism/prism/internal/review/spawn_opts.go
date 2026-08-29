package review

// spawn_opts.go — extracted SpawnOpts builder shared by Run / RunAsync.
//
// This builder is the single source of truth for the per-reviewer
// session.SpawnOpts. Both the sync `Run` and async `RunAsync` loops call
// it, so:
//
//   - the production code stays DRY (one source of truth for the audit
//     fields, the layout, and the isolation flags);
//   - the ProfileName-inheritance wiring can be unit-tested directly
//     without spinning up tmux / sidecar / a real DB write (the test
//     calls newReviewerSpawnOpts with a known activeProfile and asserts
//     the returned SpawnOpts.ProfileName matches).
//
// The builder is intentionally exhaustive: every field that
// SpawnSession reads must be present here so the production code does
// not need a second SpawnOpts literal anywhere.

import (
	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/harness"
	"github.com/prismatic-koi/prism/internal/proglog"
	"github.com/prismatic-koi/prism/internal/session"
)

// reviewerSpawnInput bundles the per-agent and round-level inputs that
// drive a single reviewer's SpawnOpts. Round-level fields are constant
// across the 5 agents in one fan-out; per-agent fields vary.
type reviewerSpawnInput struct {
	// Per-agent.
	AgentName    string
	AgentSession string
	Prompt       string

	// Round-level (constant across all 5 reviewers).
	Repo               string
	Worktree           string
	PromptTemplateHash string
	IsolationMode      string
	PluginHostPath     string
	GroupID            string
	HarnessName        string
	HarnessHandle      harness.Harness
	ModelsByRole       map[string]string
	PIExtensionDir     string

	// ProfileName is the resolved active profile for this round.
	// Set once via profile.InheritFromParent before the
	// agent loop and propagated identically to every reviewer in the
	// round — preserving the single-resolve-per-round invariant.
	ProfileName string

	// InvokerSession is the calling worker / coordinator session that
	// initiated the review fan-out (Opts.ParentSession). Constant across
	// the 5 reviewers in one fan-out. Feeds SpawnOpts.InvokerSession so
	// the durable session.spawn_intent / session.spawn_failed events
	// written by SpawnSession name the invoker in their payload, and so
	// the bus_messages audit row on the failure path is addressed to the
	// invoker rather than dropped on the floor.
	InvokerSession string
}

// newReviewerSpawnOpts returns the SpawnOpts that drives one reviewer
// session's creation. Both `Run` (sync) and `RunAsync` (async monitor
// path) call this helper so the audit row shape (spawn_inputs columns
// derived from the *Flag fields), the isolation-mode propagation, and
// the ProfileName inheritance are guaranteed identical between
// the two fan-out paths.
func newReviewerSpawnOpts(in reviewerSpawnInput) session.SpawnOpts {
	opts := session.SpawnOpts{
		InstanceID:         uuid.New().String(),
		SessionName:        in.AgentSession,
		Repo:               in.Repo,
		Worktree:           in.Worktree,
		AgentRole:          in.AgentName,
		Prompt:             in.Prompt,
		PromptSource:       "review-fanout",
		PromptTemplateHash: in.PromptTemplateHash,
		Layout:             session.LayoutAgentOnly,
		IsolationMode:      in.IsolationMode,
		PluginHostPath:     in.PluginHostPath,
		WorktreeReadOnly:   true,
		GroupID:            in.GroupID,
		RuntimeEnvVars:     in.HarnessHandle.RuntimeEnv(),
		HarnessName:        in.HarnessName,
		ModelsByRole:       in.ModelsByRole,
		// PIExtensionDir for host-mode pi launches.
		PIExtensionDir: in.PIExtensionDir,
		// spawn_inputs audit: record the per-agent role and the
		// harness so review fan-out rows are queryable alongside other
		// spawn front doors. PromptSource / PromptTemplateHash above
		// already feed the centralised SpawnSession writer.
		// IsolationFlag mirrors the resolved isolation mode so the
		// `prism stats compare` Spawn Inputs block surfaces it for
		// review-fan-out rows too.
		AgentFlag:     in.AgentName,
		HarnessFlag:   in.HarnessName,
		IsolationFlag: in.IsolationMode,
		// ProfileName carries the resolved active profile through to
		// the child's own `spawn_inputs.profile_name` row. The
		// child's runtime populatePIConfig reads
		// spawn_inputs.profile_name via the spawn_inputs chain and resolves
		// to the same profile the parent worker was spawned with,
		// instead of the host default.
		ProfileName: in.ProfileName,
		// InvokerSession is the calling worker (Opts.ParentSession)
		// so the durable spawn_intent / spawn_failed events written
		// by SpawnSession name the invoker in their payload.
		InvokerSession: in.InvokerSession,
	}
	// For socket-pipe harnesses (e.g. "pi") in host isolation mode,
	// pre-compute the Unix socket path so agentPaneEnvVars can inject
	// PRISM_HARNESS_PIPE into the tmux pane (session/session.go:778-786).
	// Without this gate the PI extension early-returns as a no-op,
	// `--agent` is never registered as a CLI flag, and pi rejects
	// `--agent review-<role> --prompt "..."` with `Unknown options`,
	// leaving the review agent idle forever.
	//
	// bwrap / sandbox-exec set PRISM_HARNESS_PIPE via their own
	// paths (bwrap.go --setenv, sandbox-exec profile); only
	// inject here for host mode. Mirrors the same block in cmd/spawn.go,
	// cmd/switch.go, cmd/restore.go, and cmd/investigate.go.
	if hShape, hShapeOK := harness.ShapeOf(in.HarnessName); hShapeOK && hShape == harness.TransportSocketPipe && in.IsolationMode == "host" {
		if pipePath, pipeErr := session.SidecarHarnessPipePath(in.AgentSession); pipeErr == nil {
			opts.HarnessPipeSockPath = pipePath
		} else {
			proglog.Warnf("[prism review] warning: could not resolve harness pipe path for %q: %v\n", in.AgentSession, pipeErr)
		}
	}
	return opts
}
