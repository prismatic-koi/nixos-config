package iris

// tool_dispatcher.go — per-tool sandboxed execution (D-4).
//
// D-3 proved the dispatch loop end-to-end with host-mode (unsandboxed)
// executors.  D-4 replaces the six file-tool executors with sandboxed
// subprocess wrappers:
//
//   - Linux:  bwrap (bubblewrap)
//   - macOS:  sandbox-exec with a generated SBPL profile
//
// The bash executor remains unsandboxed (that is D-5's scope).
//
// Each tool call runs in its own subprocess.  The tool dispatcher:
//  1. Validates the path argument (Go-side, before starting any subprocess).
//  2. Selects the executor for the named tool.
//  3. Runs the executor in a sandboxed subprocess, capturing stdout+stderr.
//  4. Streams partial output via tool_exec_update frames.
//  5. Watches for a tool_abort signal and kills the subprocess + descendants.
//  6. Returns a toolResult with success/isError/output/details.
//
// Path validation is the primary enforcement mechanism; the sandbox is
// defence-in-depth.

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// toolResult is the result of executing a tool call.
type toolResult struct {
	Success bool
	IsError bool
	Output  string
	Details map[string]any
}

// toolDispatcher holds the context for a single tool_exec dispatch.
type toolDispatcher struct {
	worktree   string
	// tmpDir is the per-session host-side backing directory for the in-sandbox
	// /tmp mount.  Computed from the harness socket path:
	//   sessionDir = filepath.Dir(sess.HarnessSockPath)
	//   tmpDir     = filepath.Join(sessionDir, "tmp")
	// Populated by the harness socket server before dispatching.
	tmpDir     string
	// role is the session's agent role ("worker", "coordinator", etc.).
	// Used by the bash sandbox to select the role-scoped GITHUB_TOKEN.
	role       string
	// bareRoot is the bare git repository root, used to derive the GitHub
	// account for the 4-PAT token selection.
	bareRoot   string
	// broker resolves per-call credentials for tool subprocesses (D-7).
	// May be nil in legacy tests; callers should populate it from the harness.
	broker     *CredentialBroker
	writer     *jsonlWriter
	abortCh    <-chan struct{}
	toolExecID string
}

// CredentialNamesForTool returns the audit-only credential names that would
// be injected into a subprocess for the named tool, given the dispatcher's
// role and bareRoot. Used by the harness socket server to populate the
// `credentials_injected` field on tool_call events before dispatch runs.
//
// The list mirrors what dispatch() will actually inject; it is computed by
// asking the broker, not by re-implementing the policy here.
func (d *toolDispatcher) CredentialNamesForTool(toolName string) []string {
	broker := d.broker
	if broker == nil {
		broker = NewCredentialBroker()
	}
	switch toolName {
	case "bash":
		return broker.ResolveBash(d.role, d.bareRoot).Names
	default:
		// File tools and any unknown tool: no credentials injected.
		return nil
	}
}

// dispatch selects and runs the appropriate tool executor for the frame.
// Returns a toolResult that is sent back as a tool_exec_result frame.
func (d *toolDispatcher) dispatch(ctx context.Context, frame ToolExecFrame) toolResult {
	switch frame.Name {
	case "bash":
		return d.runBash(ctx, frame)
	case "read":
		return d.runRead(ctx, frame)
	case "edit":
		return d.runEdit(ctx, frame)
	case "write":
		return d.runWrite(ctx, frame)
	case "grep":
		return d.runGrep(ctx, frame)
	case "find":
		return d.runFind(ctx, frame)
	case "ls":
		return d.runLs(ctx, frame)
	default:
		return toolResult{
			Success: false,
			IsError: true,
			Output:  fmt.Sprintf("iris: unknown tool %q", frame.Name),
		}
	}
}

// stringArg extracts a string argument from the args map.
func stringArg(args map[string]any, key string) string {
	v, ok := args[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// runSubprocess runs a subprocess, streams updates, and handles abort.
// cwd is the working directory; env is the subprocess environment (nil → inherit).
// Returns a toolResult.
func (d *toolDispatcher) runSubprocess(ctx context.Context, cwd string, env []string, name string, argv ...string) toolResult {
	// Build the command with a new process group so we can kill all descendants.
	cmd := exec.CommandContext(ctx, name, argv...)
	cmd.Dir = cwd
	if env != nil {
		cmd.Env = env
	}
	// Put the subprocess in its own process group so SIGTERM/-KILL reaches all
	// descendants when the abort signal fires.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Capture stdout+stderr by using a single pipe. We set a combined writer
	// by using a pair of pipes connected to the same buffer.
	// Actually, we use a simpler approach: run with combined stdout+stderr via
	// a bytes.Buffer read after Wait(), but to support streaming we use
	// os.Pipe to interleave in real time.
	pr, pw, err := os.Pipe()
	if err != nil {
		return toolResult{Success: false, IsError: true, Output: fmt.Sprintf("iris: os.Pipe: %v", err)}
	}
	cmd.Stdout = pw
	cmd.Stderr = pw
	var stdoutPipe io.Reader = pr

	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		return toolResult{Success: false, IsError: true, Output: fmt.Sprintf("iris: exec %q: %v", name, err)}
	}
	// Close the write end in the parent so EOF propagates when the child exits.
	pw.Close()
	pid := cmd.Process.Pid

	// Stream output in a goroutine, sending tool_exec_update frames.
	var buf strings.Builder
	readDone := make(chan error, 1)
	go func() {
		tmp := make([]byte, 4096)
		for {
			n, readErr := stdoutPipe.Read(tmp)
			if n > 0 {
				chunk := string(tmp[:n])
				buf.WriteString(chunk)
				// Send update frame (best-effort; ignore write errors).
				_ = d.writer.write(ToolExecUpdateFrame{
					Type:    FrameTypeToolExecUpdate,
					ID:      d.toolExecID,
					Content: chunk,
				})
			}
			if readErr != nil {
				if readErr != io.EOF {
					readDone <- readErr
				} else {
					readDone <- nil
				}
				return
			}
		}
	}()

	// Wait for either: subprocess exit, read completion, abort, or ctx cancel.
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()

	var readErr error
	var waitErr error
	select {
	case readErr = <-readDone:
		// Output fully read; now wait for the process to exit.
		waitErr = <-waitDone

	case <-d.abortCh:
		// AbortSignal fired — SIGTERM the process group, then SIGKILL after grace.
		log.Printf("[iris] tool_abort received for id=%s (pid=%d) — killing process group", d.toolExecID, pid)
		killProcessGroup(pid)
		// Drain stdout and wait for exit, but bound by a SIGKILL escalation timer
		// so a SIGTERM-resistant subprocess cannot block this goroutine forever.
		killTimer := time.NewTimer(killGracePeriod)
		defer killTimer.Stop()
		select {
		case readErr = <-readDone:
		case <-killTimer.C:
			sigkillProcessGroup(pid)
		case <-ctx.Done():
			sigkillProcessGroup(pid)
		}
		// Drain any remaining goroutines; killTimer ensures Wait unblocks.
		select {
		case <-readDone:
		default:
		}
		<-waitDone
		return toolResult{
			Success: false,
			IsError: true,
			Output:  "aborted",
		}

	case <-ctx.Done():
		// Daemon shutdown — SIGTERM then SIGKILL after grace.
		killProcessGroup(pid)
		killTimer := time.NewTimer(killGracePeriod)
		defer killTimer.Stop()
		select {
		case <-readDone:
		case <-killTimer.C:
			sigkillProcessGroup(pid)
		}
		select {
		case <-readDone:
		default:
		}
		<-waitDone
		return toolResult{
			Success: false,
			IsError: true,
			Output:  "daemon shutdown",
		}
	}

	_ = readErr // non-fatal; output captured in buf

	output := buf.String()
	success := waitErr == nil

	return toolResult{
		Success: success,
		IsError: !success,
		Output:  output,
	}
}

// killGracePeriod is the time between SIGTERM and SIGKILL when aborting a
// tool subprocess. 5 s matches the design doc's toolSubprocessKillTimeout.
const killGracePeriod = 5 * time.Second

// killProcessGroup sends SIGTERM to the entire process group of pid.
func killProcessGroup(pid int) {
	pgid := -pid
	_ = syscall.Kill(pgid, syscall.SIGTERM)
}

// sigkillProcessGroup sends SIGKILL to the entire process group of pid.
func sigkillProcessGroup(pid int) {
	pgid := -pid
	_ = syscall.Kill(pgid, syscall.SIGKILL)
}

// --- Tool executors (D-4: sandboxed subprocesses for file tools) ---

// runBash executes the bash tool in a sandbox (D-5).
//
// On Linux: bwrap with the D-5 mount set (see bash_sandbox_linux.go).
// On macOS: sandbox-exec with a generated SBPL profile (see bash_sandbox_darwin.go).
//
// The bash subprocess receives:
//   - A role-scoped GITHUB_TOKEN from the 4-PAT architecture.
//   - AWS credential file mounts (config, credentials, sso, cli).
//   - Git and SSH configuration via synthesised temp files.
//   - Network access (outbound, unrestricted).
//
// LLM API keys (ANTHROPIC_API_KEY, OPENROUTER_API_KEY) are NOT injected.
//
// Output exceeding spillThreshold is written to a spill file in the
// per-session /tmp directory (matching pi's /tmp/pi-bash-<id>.log convention).
func (d *toolDispatcher) runBash(ctx context.Context, frame ToolExecFrame) toolResult {
	command := stringArg(frame.Args, "command")
	if command == "" {
		return toolResult{Success: false, IsError: true, Output: "bash: missing 'command' argument"}
	}

	// Role-keyed permission check (D-10 parity gate). Coordinator-only
	// commands like `prism merge` are denied for non-coordinator roles
	// before the subprocess is spawned. See internal/iris/bash_permission.go.
	if allowed, reason := CheckBashPermission(d.role, command); !allowed {
		return toolResult{Success: false, IsError: true, Output: reason}
	}

	// Run in the OS-specific sandbox.
	result := d.runBashInSandbox(ctx, command)

	// Apply spill semantics: write oversized output to a temp file.
	if result.Success || result.Output != "" {
		spilled, details := maybeSpill(result.Output, d.toolExecID, d.tmpDir)
		result.Output = spilled
		if details != nil {
			if result.Details == nil {
				result.Details = details
			} else {
				for k, v := range details {
					result.Details[k] = v
				}
			}
		}
	}

	return result
}

// runRead executes the read tool in a sandboxed subprocess (worktree RO).
func (d *toolDispatcher) runRead(ctx context.Context, frame ToolExecFrame) toolResult {
	filePath := stringArg(frame.Args, "file_path")
	if filePath == "" {
		filePath = stringArg(frame.Args, "path")
	}
	if filePath == "" {
		return toolResult{Success: false, IsError: true, Output: "read: missing 'file_path' argument"}
	}

	// Path validation — primary enforcement (sandbox is defence-in-depth).
	resolved, err := validateToolPath(d.worktree, d.tmpDir, filePath)
	if err != nil {
		return toolResult{Success: false, IsError: true, Output: fmt.Sprintf("read: %v", err)}
	}

	// Build argv: cat for reading (handles binary; offset/limit applied in Go
	// after capture).  We cat the resolved path so the subprocess sees the
	// symlink-resolved location — the sandbox bind-mount covers the resolved
	// location, so the cat will succeed.
	output, ok, sandboxErr := d.runInFileSandbox(ctx, true, "cat", resolved)
	if sandboxErr != nil {
		return toolResult{Success: false, IsError: true, Output: fmt.Sprintf("read: sandbox: %v", sandboxErr)}
	}
	if !ok {
		return toolResult{Success: false, IsError: true, Output: fmt.Sprintf("read: %s", output)}
	}

	content := output

	// Apply offset/limit if provided (applied after capture in Go).
	offset := intArg(frame.Args, "offset")
	limit := intArg(frame.Args, "limit")
	if offset > 0 || limit > 0 {
		lines := strings.Split(content, "\n")
		start := offset
		if start < 0 {
			start = 0
		}
		if start >= len(lines) {
			content = ""
		} else {
			end := len(lines)
			if limit > 0 && start+limit < end {
				end = start + limit
			}
			content = strings.Join(lines[start:end], "\n")
		}
	}

	return toolResult{Success: true, IsError: false, Output: content}
}

// runEdit executes the edit tool in a sandboxed subprocess (worktree RW).
func (d *toolDispatcher) runEdit(ctx context.Context, frame ToolExecFrame) toolResult {
	filePath := stringArg(frame.Args, "file_path")
	if filePath == "" {
		filePath = stringArg(frame.Args, "path")
	}
	oldText := stringArg(frame.Args, "old_string")
	newText := stringArg(frame.Args, "new_string")
	if filePath == "" {
		return toolResult{Success: false, IsError: true, Output: "edit: missing 'file_path' argument"}
	}

	// Path validation — primary enforcement.
	resolved, err := validateToolPath(d.worktree, d.tmpDir, filePath)
	if err != nil {
		return toolResult{Success: false, IsError: true, Output: fmt.Sprintf("edit: %v", err)}
	}

	// Read the file content in a RO sandbox, then perform the edit in-process,
	// then write the result via a RW sandbox.  This keeps the actual file
	// manipulation logic in Go (clear error messages, count checks) while
	// ensuring both reads and writes go through the sandbox path.
	readOutput, readOK, sandboxErr := d.runInFileSandbox(ctx, true, "cat", resolved)
	if sandboxErr != nil {
		return toolResult{Success: false, IsError: true, Output: fmt.Sprintf("edit: sandbox read: %v", sandboxErr)}
	}
	if !readOK {
		return toolResult{Success: false, IsError: true, Output: fmt.Sprintf("edit: read: %s", readOutput)}
	}

	content := readOutput
	if oldText != "" {
		count := strings.Count(content, oldText)
		if count == 0 {
			return toolResult{Success: false, IsError: true, Output: "edit: old_string not found in file"}
		}
		if count > 1 {
			return toolResult{
				Success: false,
				IsError: true,
				Output:  fmt.Sprintf("edit: old_string appears %d times in file; it must be unique for a safe replacement", count),
			}
		}
	}
	var newContent string
	if oldText == "" {
		newContent = newText
	} else {
		newContent = strings.Replace(content, oldText, newText, 1)
	}

	// Write the new content via a sandboxed tee/dd into the resolved path.
	// We use a shell invocation via the RW sandbox to write the file.
	// The content is passed via stdin.
	writeOutput, writeOK, sandboxErr := d.runInFileSandboxWithStdin(ctx, false, newContent, "sh", "-c", "cat > "+shellQuote(resolved))
	if sandboxErr != nil {
		return toolResult{Success: false, IsError: true, Output: fmt.Sprintf("edit: sandbox write: %v", sandboxErr)}
	}
	if !writeOK {
		return toolResult{Success: false, IsError: true, Output: fmt.Sprintf("edit: write: %s", writeOutput)}
	}

	return toolResult{Success: true, IsError: false, Output: "file edited successfully"}
}

// runWrite executes the write tool in a sandboxed subprocess (worktree RW).
func (d *toolDispatcher) runWrite(ctx context.Context, frame ToolExecFrame) toolResult {
	filePath := stringArg(frame.Args, "file_path")
	if filePath == "" {
		filePath = stringArg(frame.Args, "path")
	}
	content := stringArg(frame.Args, "content")
	if filePath == "" {
		return toolResult{Success: false, IsError: true, Output: "write: missing 'file_path' argument"}
	}

	// Path validation — primary enforcement.
	_, err := validateToolPath(d.worktree, d.tmpDir, filePath)
	if err != nil {
		return toolResult{Success: false, IsError: true, Output: fmt.Sprintf("write: %v", err)}
	}

	// Resolve to absolute for the sandboxed write.
	abs := filePath
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(d.worktree, filePath)
	}
	abs = filepath.Clean(abs)

	// The parent directory may not exist; create it on the host before entering
	// the sandbox (the sandbox's worktree RW bind-mount allows directory creation,
	// but mkdir -p is simpler to do in Go).
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return toolResult{Success: false, IsError: true, Output: fmt.Sprintf("write: mkdir: %v", err)}
	}

	// Write via a sandboxed subprocess with content on stdin.
	writeOutput, writeOK, sandboxErr := d.runInFileSandboxWithStdin(ctx, false, content, "sh", "-c", "cat > "+shellQuote(abs))
	if sandboxErr != nil {
		return toolResult{Success: false, IsError: true, Output: fmt.Sprintf("write: sandbox: %v", sandboxErr)}
	}
	if !writeOK {
		return toolResult{Success: false, IsError: true, Output: fmt.Sprintf("write: %s", writeOutput)}
	}
	return toolResult{Success: true, IsError: false, Output: "file written successfully"}
}

// runGrep executes the grep tool in a sandboxed subprocess (worktree RO).
func (d *toolDispatcher) runGrep(ctx context.Context, frame ToolExecFrame) toolResult {
	pattern := stringArg(frame.Args, "pattern")
	path := stringArg(frame.Args, "path")
	if pattern == "" {
		return toolResult{Success: false, IsError: true, Output: "grep: missing 'pattern' argument"}
	}

	target := d.worktree
	if path != "" {
		// Validate the path argument.
		resolved, err := validateToolPath(d.worktree, d.tmpDir, path)
		if err != nil {
			return toolResult{Success: false, IsError: true, Output: fmt.Sprintf("grep: %v", err)}
		}
		target = resolved
	}

	output, ok, sandboxErr := d.runInFileSandbox(ctx, true, "grep", "-rn", "--", pattern, target)
	if sandboxErr != nil {
		return toolResult{Success: false, IsError: true, Output: fmt.Sprintf("grep: sandbox: %v", sandboxErr)}
	}
	// grep exits 1 when no matches found — treat as success with empty output.
	if !ok && output == "" {
		return toolResult{Success: true, IsError: false, Output: ""}
	}
	return toolResult{Success: ok || output != "", IsError: false, Output: output}
}

// runFind executes the find tool in a sandboxed subprocess (worktree RO).
func (d *toolDispatcher) runFind(ctx context.Context, frame ToolExecFrame) toolResult {
	path := stringArg(frame.Args, "path")
	if path == "" {
		path = "."
	}

	// Validate the path argument.
	resolved, err := validateToolPath(d.worktree, d.tmpDir, path)
	if err != nil {
		return toolResult{Success: false, IsError: true, Output: fmt.Sprintf("find: %v", err)}
	}

	var argv []string
	argv = append(argv, resolved)
	if name := stringArg(frame.Args, "name"); name != "" {
		argv = append(argv, "-name", name)
	}
	if typeFilter := stringArg(frame.Args, "type"); typeFilter != "" {
		argv = append(argv, "-type", typeFilter)
	}

	output, ok, sandboxErr := d.runInFileSandbox(ctx, true, "find", argv...)
	if sandboxErr != nil {
		return toolResult{Success: false, IsError: true, Output: fmt.Sprintf("find: sandbox: %v", sandboxErr)}
	}
	return toolResult{Success: ok, IsError: !ok, Output: output}
}

// runLs executes the ls tool in a sandboxed subprocess (worktree RO).
func (d *toolDispatcher) runLs(ctx context.Context, frame ToolExecFrame) toolResult {
	path := stringArg(frame.Args, "path")
	if path == "" {
		path = "."
	}

	// Validate the path argument.
	resolved, err := validateToolPath(d.worktree, d.tmpDir, path)
	if err != nil {
		return toolResult{Success: false, IsError: true, Output: fmt.Sprintf("ls: %v", err)}
	}

	output, ok, sandboxErr := d.runInFileSandbox(ctx, true, "ls", "-la", resolved)
	if sandboxErr != nil {
		return toolResult{Success: false, IsError: true, Output: fmt.Sprintf("ls: sandbox: %v", sandboxErr)}
	}
	return toolResult{Success: ok, IsError: !ok, Output: output}
}

// shellQuote wraps s in single quotes, escaping embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// resolveWorktreePath resolves a (potentially relative) path against the
// worktree root. Absolute paths are returned as-is.
func resolveWorktreePath(worktree, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(worktree, path)
}

// intArg extracts an integer argument from the args map (returns 0 when absent).
func intArg(args map[string]any, key string) int {
	v, ok := args[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}


