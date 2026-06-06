package session

// Multiplexer-routed session layouts (issue #2158).
//
// When PRISM_USE_MUX=1 is set at the CLI gate, SpawnSession dispatches
// here instead of spawnFullLayout / spawnAgentOnlyLayout. The mux path
// replaces the four tmux primitives at the bottom of the spawn flow
// — NewSessionDetached, NewWindow×3, SendKeys for the initial prompt —
// with a registration call to prismd-mux and one Panes().Create call
// per pane.
//
// Design notes:
//
//   - The DB ordering above this file is unchanged (issue contract:
//     "Allocate port via DB — unchanged"). spawn_inputs, sessions,
//     and agent_status are seeded by SpawnSession before we get here.
//
//   - The sidecar is started via the same StartSidecarWithOpts call
//     the tmux path uses (issue contract: "StartSidecar (unchanged
//     — sidecar is multiplexer-independent)"). The mux daemon owns
//     the *terminal* substrate, not the *agent* substrate.
//
//   - The initial prompt is delivered via Panes().SendInput on the
//     `agent` pane after the sidecar is up. This mirrors the tmux
//     path's SendKeys-on-the-agent-window approach. Because the
//     agent pane spawns the `prism agent-run` binary (in sandboxed
//     modes) or the harness binary directly (in host mode), the
//     bytes go to its stdin — same surface tmux's SendKeys writes to.
//
//   - On any failure mid-flight we tear down whatever was created.
//     SpawnSession's outer-level handler then runs its own cleanup
//     (mark agent_status as error, release port, etc.) so a failed
//     spawn does not leave a half-alive session.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/prismatic-koi/prism/internal/mux/client"
	"github.com/prismatic-koi/prism/internal/mux/pane"
)

// muxClientTimeout bounds a single round-trip to the mux daemon.
// Mirrors the cmd-side mux helper's choice; we duplicate the constant
// here so the session package does not import cmd/.
const muxLayoutClientTimeout = 5 * time.Second

// muxLayoutPanes is the ordered list of panes the LayoutFull (3-window)
// shape registers in the mux daemon. The order matches the tmux path's
// window indices: 0 edit, 1 agent, 2 term. ActivePane defaults to
// "agent" so the renderer focuses the agent pane on first render.
var muxLayoutFullPanes = []string{"edit", "agent", "term"}

// muxLayoutAgentOnlyPanes is the 2-pane equivalent used by review and
// other LayoutAgentOnly callers. Order matches the tmux path: 0 shell,
// 1 agent. The renderer focuses agent.
var muxLayoutAgentOnlyPanes = []string{"shell", "agent"}

// muxClientFactory is the indirection point tests use to swap the
// canonical client.New() call for one that points at a t.TempDir()
// socket. Production callers leave this at the default; tests in
// mux_layout_test.go override it via overrideMuxClientFactory.
//
// The var is package-private to keep the cutover surface small —
// CLI callers always go through the canonical path.
var muxClientFactory = func() (client.MuxClient, error) {
	return client.New(client.WithTimeout(muxLayoutClientTimeout))
}

// SpawnMuxLayout is the PRISM_USE_MUX=1 dispatch target for
// SpawnSession. It registers the session and its panes in the mux
// daemon, starts the sidecar, waits for readiness (when the layout
// expects it), and delivers the initial prompt via Panes().SendInput.
//
// On error the function tears down whatever it created and returns
// the underlying client error. SpawnSession's caller is responsible
// for the DB-side cleanup (mark agent_status as error, release port,
// etc.) — the same shape the tmux path uses.
//
// The opts struct is the same SpawnOpts SpawnSession received; we
// re-derive the layout-specific details (pane names, active pane)
// from opts.Layout. opts.InstanceID is non-empty by this point
// (SpawnSession mints it before calling us).
func SpawnMuxLayout(opts SpawnOpts, port int) error {
	if opts.SessionName == "" {
		return fmt.Errorf("spawn mux layout: SessionName is required")
	}
	if opts.Worktree == "" {
		return fmt.Errorf("spawn mux layout: Worktree is required")
	}

	paneNames, activePane, err := muxPanesForLayout(opts.Layout)
	if err != nil {
		return err
	}

	mc, err := muxClientFactory()
	if err != nil {
		return fmt.Errorf("spawn mux layout: new client: %w", err)
	}
	defer mc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 1. Register the session shell with no embedded panes. The panes
	//    are added below via Panes().Create so each one can carry its
	//    own runtime side (argv, cwd, env). Embedding the panes in
	//    Sessions().Create would create model-only rows that conflict
	//    with the runtime-side Create below, forcing a destroy/recreate
	//    cycle that reorders panes and clobbers ActivePane.
	if _, err := mc.Sessions().Create(ctx, pane.Session{
		ID:        opts.SessionName,
		Repo:      opts.Repo,
		Branch:    opts.BranchFlag,
		Worktree:  opts.Worktree,
		AgentRole: opts.AgentRole,
	}); err != nil {
		// Already-registered (e.g. a re-spawn after a crash that
		// left the daemon's session tree intact) is recoverable —
		// the caller has already serialised on spawnLock so this
		// can only be a stale row. Best-effort destroy + retry.
		if errors.Is(err, client.ErrSessionExists) {
			if destErr := mc.Sessions().Destroy(ctx, opts.SessionName); destErr != nil {
				return fmt.Errorf("spawn mux layout: stale session destroy: %w", destErr)
			}
			if _, retryErr := mc.Sessions().Create(ctx, pane.Session{
				ID:        opts.SessionName,
				Repo:      opts.Repo,
				Branch:    opts.BranchFlag,
				Worktree:  opts.Worktree,
				AgentRole: opts.AgentRole,
			}); retryErr != nil {
				return fmt.Errorf("spawn mux layout: session create (after stale destroy): %w", retryErr)
			}
		} else {
			return fmt.Errorf("spawn mux layout: session create: %w", err)
		}
	}

	// 2. Add each pane with its argv. The model auto-activates the
	//    first pane (AddPane sets ActivePane when the session had
	//    none), so the canonical order matters — the slice order is
	//    the order we add.
	//
	//    The agent pane's argv is branched on isolation mode (#2176):
	//    sandboxed modes (bwrap / podman / sandbox-exec) exec
	//    `prism agent-run --session <name>`; host mode execs the
	//    harness directly via BuildAgentCmd. Both paths wrap the
	//    command string in `sh -c …` so shell features (env-var
	//    expansion, `$(cat <prompt-file>)`, the podman readiness
	//    wait loop) keep working — mirroring tmux's NewWindow,
	//    which itself runs the agent command via `sh -c`.
	isolation := resolveLayoutIsolationMode(opts)
	for _, name := range paneNames {
		paneOpts := muxPaneOptsFor(opts, port, name, isolation)
		if err := mc.Panes().Create(ctx, opts.SessionName, name, paneOpts); err != nil {
			// Roll back: tear down the session so a retry sees a
			// clean slate in the daemon's session tree.
			_ = mc.Sessions().Destroy(ctx, opts.SessionName)
			return fmt.Errorf("spawn mux layout: register pane %q: %w", name, err)
		}
	}

	// 3. Set the canonical active pane explicitly. AddPane's
	//    auto-activation picked the FIRST pane registered (edit /
	//    shell) but the soak expects the renderer to focus the agent
	//    on first paint.
	if activePane != "" {
		if _, err := mc.Panes().Switch(ctx, client.PaneSwitchRequest{
			SessionID: opts.SessionName,
			Name:      activePane,
		}); err != nil {
			// Non-fatal: the panes are registered and the PTYs are
			// spawning. Surface the warning but keep going so the
			// spawn does not fail on a focus glitch.
			fmt.Fprintf(os.Stderr,
				"warning: could not set active pane %q for %q: %v\n",
				activePane, opts.SessionName, err)
		}
	}

	// 2. Start the sidecar — same call shape as the tmux path. The
	//    sidecar is multiplexer-independent (issue contract).
	sidecarOpts := StartSidecarOpts{
		Port:           port,
		IsolationMode:  resolveLayoutIsolationMode(opts),
		AgentRole:      opts.AgentRole,
		Worktree:       opts.Worktree,
		PluginHostPath: opts.PluginHostPath,
		InitialPrompt:  opts.Prompt,
		ConfigContent:  opts.ConfigContent,
		InstanceID:     opts.InstanceID,
		HarnessName:    opts.HarnessName,
		ModelsByRole:   opts.ModelsByRole,
	}
	if err := StartSidecarWithOpts(opts.SessionName, sidecarOpts); err != nil {
		// Match the tmux path's warning-and-continue semantics: a
		// sidecar failure does not abort the spawn — the session is
		// already up. Surface the error so it is visible in the log
		// but do not return it (the spawn is still nominally
		// successful from the operator's viewpoint).
		fmt.Fprintf(os.Stderr, "warning: could not start sidecar for %q: %v\n",
			opts.SessionName, err)
	}

	// 3. Initial prompt delivery. The host-mode tmux path delivers
	//    the prompt via $(cat <file>) inside the agent launch command;
	//    we cannot do that under the mux path because the daemon's
	//    pane.create runs the argv directly — there is no tmux to
	//    expand the shell substitution. Instead, write the prompt
	//    bytes (with a trailing newline) to the agent pane's stdin
	//    once the sidecar is up.
	//
	//    For the host mode + LayoutFull case the agent is the harness
	//    binary directly; for sandboxed modes it is `prism agent-run`
	//    which reads PRISM_INITIAL_PROMPT_FILE from its env. Either
	//    way the prompt has been written to the per-session run dir
	//    by SpawnSession (#1064/#1092), so the SendInput here is the
	//    safety-net path. We always write so a tmux-vs-mux behavioural
	//    diff stays bounded by the agent's own prompt-read shape, not
	//    by the layout substrate.
	if opts.Prompt != "" {
		// Add a trailing newline so `read -r` in the agent shell
		// returns. The agent binary itself does not consume stdin
		// for this purpose; the SendInput is the trigger for the
		// outer shell to flush.
		if err := mc.Panes().SendInput(ctx, opts.SessionName, "agent", opts.Prompt+"\n"); err != nil {
			fmt.Fprintf(os.Stderr,
				"warning: could not deliver initial prompt to mux pane %q: %v\n",
				opts.SessionName, err)
		}
	}
	return nil
}

// muxPanesForLayout returns the canonical pane list and active pane
// for the given Layout. LayoutFull and LayoutAgentOnly are the only
// supported layouts under the mux path; LayoutBare and LayoutScratchpad
// have no agent and would not have been routed through SpawnSession
// in the first place.
func muxPanesForLayout(layout Layout) (panes []string, active string, err error) {
	switch layout {
	case LayoutFull:
		return muxLayoutFullPanes, "agent", nil
	case LayoutAgentOnly:
		return muxLayoutAgentOnlyPanes, "agent", nil
	default:
		return nil, "", fmt.Errorf("mux layout: unsupported layout %d", layout)
	}
}

// muxPaneOptsFor returns the PaneCreateOptions for the named pane.
// Each pane's argv mirrors the tmux path's equivalent window:
//
//   - edit   → /bin/sh under the worktree (matches tmux
//              setupFullLayout window 0; the renderer will gain a
//              "switch to edit pane" key that opens nvim on demand —
//              not strictly needed for the soak)
//   - agent  → the harness launch command, dispatched by isolation:
//              · sandboxed (bwrap / podman / sandbox-exec):
//                `prism agent-run --session <name>` (BuildAgentCmd's
//                AgentPaneCmd output for those modes; #2176 cutover)
//              · host: pi (or the configured harness) execs directly
//                via buildDirectAgentCmd's output (#2176 cutover)
//   - term   → /bin/sh (matches tmux setupFullLayout window 2)
//   - shell  → /bin/sh (LayoutAgentOnly window 0)
//
// The agent pane is wrapped in `sh -c <cmd>` so shell features the
// tmux path relies on — host mode's `$(cat <prompt-file>)`,
// sandboxed modes' env-var expansion, and podman's readiness-wait
// loop — keep working. This mirrors tmux's NewWindow, which runs the
// agent command via `sh -c` itself.
//
// The Cwd is opts.Worktree for every pane so the shell prompt
// matches the tmux path's per-window starting directory.
func muxPaneOptsFor(opts SpawnOpts, port int, name, isolation string) client.PaneCreateOptions {
	shell := defaultShellPath()
	cwd := opts.Worktree

	switch name {
	case "agent":
		// Build the agent launch command using the same Opts shape
		// the tmux path uses (buildOptsForLayout). The isolation
		// mode is explicit so a SpawnOpts with an empty IsolationMode
		// (the review-fanout shape on a bwrap-default machine)
		// resolves the same way it does in the tmux path —
		// resolveLayoutIsolationMode is called once in SpawnMuxLayout
		// and threaded through here.
		launchOpts := buildOptsForLayout(opts, port, opts.PromptFilePath)
		launchOpts.IsolationMode = isolation
		launchOpts.Worktree = opts.Worktree
		agentCmd := BuildAgentCmd(launchOpts)

		return client.PaneCreateOptions{
			// `sh -c <cmd>` mirrors what tmux does for NewWindow's
			// command argument. argv[0] is the shell so the daemon's
			// PTY-spawn path is unchanged.
			Argv: []string{shell, "-c", agentCmd},
			Cwd:  cwd,
			// Merge the daemon's env (inherited via os.Environ() on
			// the CLI side — both daemon and CLI run under the same
			// user session for the soak) with the per-session
			// overrides agentPaneEnvVars produces. The overrides
			// carry PRISM_INITIAL_PROMPT_FILE (sandboxed modes) and
			// PRISM_HARNESS_PIPE (host mode with a socket-pipe
			// harness) — without these the agent never reads the
			// prompt and never connects to the sidecar bridge.
			Env:  muxAgentEnv(launchOpts),
			Cols: 80,
			Rows: 24,
		}
	case "edit", "term", "shell":
		return client.PaneCreateOptions{
			Argv: []string{shell},
			Cwd:  cwd,
			Env:  nil, // inherit daemon env
			Cols: 80,
			Rows: 24,
		}
	default:
		// Defensive: unknown pane name → still run a shell so the
		// pane has a live PTY rather than an undefined runtime.
		return client.PaneCreateOptions{
			Argv: []string{shell},
			Cwd:  cwd,
			Env:  nil,
			Cols: 80,
			Rows: 24,
		}
	}
}

// muxAgentEnv builds the env map for the agent pane: the CLI process's
// own os.Environ() (used as a proxy for the daemon's inherited env,
// since both run under the same user for the soak) merged with the
// per-session overrides from agentPaneEnvVars.
//
// The mux daemon's pane.create REPLACES the child's env when a non-nil
// map is supplied — there is no "inherit + override" mode — so we have
// to assemble the full set here. agentPaneEnvVars overrides win on key
// collision: a session-specific PRISM_HARNESS_PIPE replaces any stale
// one in the operator's shell env.
func muxAgentEnv(opts Opts) map[string]string {
	env := make(map[string]string, len(os.Environ()))
	for _, kv := range os.Environ() {
		if i := indexByte(kv, '='); i > 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	for k, v := range agentPaneEnvVars(opts) {
		env[k] = v
	}
	return env
}

// indexByte is a tiny inline strings.IndexByte so this file does not
// need to import strings for a single call site.
func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// defaultShellPath returns the absolute path to /bin/sh, the one
// shell every Unix has. We do not consult $SHELL because the daemon
// may run with a stripped env and the user's shell may not be on
// PATH in the daemon's context.
func defaultShellPath() string {
	for _, candidate := range []string{"/bin/sh", "/usr/bin/sh"} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	// Last-resort fallback: just "sh" and hope the daemon's PATH
	// finds it. This never fires in practice on prism's supported
	// platforms (Linux + Darwin both have /bin/sh).
	return "sh"
}

// TeardownMuxSession is the mux-path equivalent of tmux.KillSession.
// It instructs the daemon to remove the session (which cascades to
// every pane's PTY via the registry) and returns any client error.
//
// Used by cmd/cleanup_mux.go and (indirectly via SpawnSession's error
// path) by the spawn cutover gate. ErrSessionNotFound is treated as
// a successful no-op so a re-cleanup is safe.
func TeardownMuxSession(sessionName string) error {
	mc, err := muxClientFactory()
	if err != nil {
		return fmt.Errorf("teardown mux session: new client: %w", err)
	}
	defer mc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := mc.Sessions().Destroy(ctx, sessionName); err != nil {
		if errors.Is(err, client.ErrSessionNotFound) {
			return nil
		}
		return err
	}
	return nil
}
