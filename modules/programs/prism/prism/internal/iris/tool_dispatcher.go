package iris

// tool_dispatcher.go — host-mode tool execution for D-3.
//
// In D-3 all tool subprocesses run directly on the host with no sandbox.
// This is the deliberate intermediate state before D-4 (worktree-scoped
// subprocess) and D-5 (bash restricted subprocess). The point is to prove the
// dispatch loop end-to-end; sandboxing is layered on later without changing
// the protocol.
//
// Each tool call runs in its own subprocess. The tool dispatcher:
//  1. Selects the executor for the named tool.
//  2. Runs the executor in a subprocess, capturing stdout+stderr.
//  3. Streams partial output via tool_exec_update frames.
//  4. Watches for a tool_abort signal and kills the subprocess + descendants.
//  5. Returns a toolResult with success/isError/output/details.

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
	writer     *jsonlWriter
	abortCh    <-chan struct{}
	toolExecID string
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

// --- Tool executors (D-3: host-mode, no sandbox) ---

func (d *toolDispatcher) runBash(ctx context.Context, frame ToolExecFrame) toolResult {
	command := stringArg(frame.Args, "command")
	if command == "" {
		return toolResult{Success: false, IsError: true, Output: "bash: missing 'command' argument"}
	}
	return d.runSubprocess(ctx, d.worktree, nil, "bash", "-c", command)
}

func (d *toolDispatcher) runRead(ctx context.Context, frame ToolExecFrame) toolResult {
	filePath := stringArg(frame.Args, "file_path")
	if filePath == "" {
		filePath = stringArg(frame.Args, "path")
	}
	if filePath == "" {
		return toolResult{Success: false, IsError: true, Output: "read: missing 'file_path' argument"}
	}
	abs := resolveWorktreePath(d.worktree, filePath)
	data, err := os.ReadFile(abs)
	if err != nil {
		return toolResult{Success: false, IsError: true, Output: fmt.Sprintf("read: %v", err)}
	}
	content := string(data)

	// Apply offset/limit if provided.
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

	abs := resolveWorktreePath(d.worktree, filePath)
	data, err := os.ReadFile(abs)
	if err != nil {
		return toolResult{Success: false, IsError: true, Output: fmt.Sprintf("edit: read: %v", err)}
	}
	content := string(data)
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
	if err := os.WriteFile(abs, []byte(newContent), 0o644); err != nil {
		return toolResult{Success: false, IsError: true, Output: fmt.Sprintf("edit: write: %v", err)}
	}
	return toolResult{Success: true, IsError: false, Output: "file edited successfully"}
}

func (d *toolDispatcher) runWrite(ctx context.Context, frame ToolExecFrame) toolResult {
	filePath := stringArg(frame.Args, "file_path")
	if filePath == "" {
		filePath = stringArg(frame.Args, "path")
	}
	content := stringArg(frame.Args, "content")
	if filePath == "" {
		return toolResult{Success: false, IsError: true, Output: "write: missing 'file_path' argument"}
	}

	abs := resolveWorktreePath(d.worktree, filePath)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return toolResult{Success: false, IsError: true, Output: fmt.Sprintf("write: mkdir: %v", err)}
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return toolResult{Success: false, IsError: true, Output: fmt.Sprintf("write: %v", err)}
	}
	return toolResult{Success: true, IsError: false, Output: "file written successfully"}
}

func (d *toolDispatcher) runGrep(ctx context.Context, frame ToolExecFrame) toolResult {
	pattern := stringArg(frame.Args, "pattern")
	path := stringArg(frame.Args, "path")
	if pattern == "" {
		return toolResult{Success: false, IsError: true, Output: "grep: missing 'pattern' argument"}
	}
	target := d.worktree
	if path != "" {
		target = resolveWorktreePath(d.worktree, path)
	}
	args := []string{"-rn", "--", pattern, target}
	return d.runSubprocess(ctx, d.worktree, nil, "grep", args...)
}

func (d *toolDispatcher) runFind(ctx context.Context, frame ToolExecFrame) toolResult {
	path := stringArg(frame.Args, "path")
	if path == "" {
		path = "."
	}
	target := resolveWorktreePath(d.worktree, path)

	var argv []string
	argv = append(argv, target)

	// Optional name pattern filter.
	if name := stringArg(frame.Args, "name"); name != "" {
		argv = append(argv, "-name", name)
	}
	if typeFilter := stringArg(frame.Args, "type"); typeFilter != "" {
		argv = append(argv, "-type", typeFilter)
	}

	return d.runSubprocess(ctx, d.worktree, nil, "find", argv...)
}

func (d *toolDispatcher) runLs(ctx context.Context, frame ToolExecFrame) toolResult {
	path := stringArg(frame.Args, "path")
	if path == "" {
		path = "."
	}
	target := resolveWorktreePath(d.worktree, path)
	return d.runSubprocess(ctx, d.worktree, nil, "ls", "-la", target)
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


