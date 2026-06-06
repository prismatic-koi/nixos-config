// Non-interactive corpus walk. For each app in corpus.toml, opens a PTY,
// hosts the argv under x/vt, drives the trigger keystrokes, captures the
// final VT state to <out>/<app>/.
package cmd

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prismatic-koi/nixos-config/modules/programs/prism/mux-spike/internal/corpus"
	"github.com/prismatic-koi/nixos-config/modules/programs/prism/mux-spike/internal/pty"
	"github.com/prismatic-koi/nixos-config/modules/programs/prism/mux-spike/internal/render"
	"github.com/prismatic-koi/nixos-config/modules/programs/prism/mux-spike/internal/vt"
)

// Corpus implements `mux-spike corpus --out <dir>`.
func Corpus(args []string) error {
	fs := flag.NewFlagSet("corpus", flag.ContinueOnError)
	out := fs.String("out", "/tmp/mux-spike-report", "directory for captures")
	manifest := fs.String("manifest", "corpus/corpus.toml", "TOML manifest path")
	only := fs.String("only", "", "comma-separated list of app names to run (default: all)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: mux-spike corpus [--out DIR] [--manifest PATH] [--only A,B,...]")
	}

	m, err := corpus.Load(*manifest)
	if err != nil {
		return err
	}

	onlyset := map[string]bool{}
	if *only != "" {
		for _, name := range splitCSV(*only) {
			onlyset[name] = true
		}
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}

	var ran, failed int
	for _, app := range m.Apps {
		if len(onlyset) > 0 && !onlyset[app.Name] {
			continue
		}
		ran++
		fmt.Fprintf(os.Stderr, "[corpus] %s ...\n", app.Name)
		appDir := filepath.Join(*out, app.Name, "xvt")
		if err := runOneApp(app, appDir); err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "[corpus] %s: %v\n", app.Name, err)
		}
	}
	fmt.Fprintf(os.Stderr, "[corpus] %d apps run, %d failed\n", ran, failed)
	return nil
}

// runOneApp walks one corpus entry end-to-end. Panics inside the engine
// are caught and recorded in meta.json with a stack trace — the walk
// continues to the remaining apps.
func runOneApp(app corpus.App, appDir string) (rerr error) {
	start := time.Now()
	meta := render.Meta{
		App:                 app.Name,
		Cols:                app.Cols,
		Rows:                app.Rows,
		SettleMs:            app.SettleMs,
		PostTriggerSettleMs: app.PostTriggerSettleMs,
		Notes:               app.Notes,
	}

	// Catch panics from anywhere in the engine pump so the walk continues.
	defer func() {
		if r := recover(); r != nil {
			meta.PanicCaught = fmt.Sprintf("%v", r)
			meta.PanicStack = string(debug.Stack())
			meta.WallDurationMs = time.Since(start).Milliseconds()
			// Best-effort meta write even on panic.
			_ = os.MkdirAll(appDir, 0o755)
			_ = render.Write(appDir, vt.Snapshot{Cols: app.Cols, Rows: app.Rows}, "", meta)
			rerr = fmt.Errorf("panic: %v", r)
		}
	}()

	if len(app.Argv) == 0 {
		return errors.New("empty argv")
	}
	if app.Cols == 0 || app.Rows == 0 {
		return errors.New("manifest entry missing cols/rows")
	}

	sess, err := pty.Start(app.Argv, uint16(app.Cols), uint16(app.Rows))
	if err != nil {
		return fmt.Errorf("pty start: %w", err)
	}
	defer sess.Close()

	host := vt.New(app.Cols, app.Rows)

	// Drain emulator-→-PTY responses (DSR replies, mode queries). The TUI
	// would deadlock without this — x/vt's response pipe blocks once its
	// internal buffer fills. See vt.Host.DrainResponses.
	go func() { _ = host.DrainResponses(sess.File) }()

	// PTY → emulator pump with byte accounting + frame count.
	//
	// `pause` is used to freeze the pump before snapshotting. The pump
	// checks pause before each Feed, and waits on `resume` while paused.
	// This is necessary because streaming TUIs (nvim, htop) keep the
	// host mutex churning; Go's sync.Mutex is not fair and a concurrent
	// Snapshot starves indefinitely.
	var (
		bytesRead int64
		frames    int
		pumpDone  = make(chan struct{})
		pumpMu    sync.Mutex
		pause     atomic.Bool
		paused    = make(chan struct{}, 1)
		resume    = make(chan struct{})
	)
	go func() {
		defer close(pumpDone)
		buf := make([]byte, 32*1024)
		for {
			n, err := sess.File.Read(buf)
			if n > 0 {
				if pause.Load() {
					select {
					case paused <- struct{}{}:
					default:
					}
					<-resume
				}
				pumpMu.Lock()
				bytesRead += int64(n)
				frames++
				pumpMu.Unlock()
				if _, werr := host.Feed(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	dbg("[%s] pty up, child pid=%d", app.Name, sess.Cmd.Process.Pid)

	// 1. Initial settle.
	if !sleepOrDone(time.Duration(app.SettleMs)*time.Millisecond, pumpDone) {
		// Process exited during settle — capture what we have and bail.
		snap := host.Snapshot()
		ansi := host.Render()
		pumpMu.Lock()
		meta.BytesFromPTY, meta.FrameCount = bytesRead, frames
		pumpMu.Unlock()
		meta.WallDurationMs = time.Since(start).Milliseconds()
		meta.ExitCode = sessExitCode(sess)
		return render.Write(appDir, snap, ansi, meta)
	}

	dbg("[%s] initial settle done", app.Name)

	// 2. Drive triggers.
	for _, trig := range app.Triggers {
		bs := corpus.ExpandKeystroke(trig)
		if _, werr := sess.File.Write(bs); werr != nil {
			break
		}
		meta.TriggersSent++
		// Small inter-trigger gap so apps can react.
		time.Sleep(80 * time.Millisecond)
	}

	dbg("[%s] %d triggers sent", app.Name, meta.TriggersSent)

	// 3. Post-trigger settle.
	sleepOrDone(time.Duration(app.PostTriggerSettleMs)*time.Millisecond, pumpDone)

	dbg("[%s] post-trigger settle done; pausing pump", app.Name)

	// 4. Pause the pump and wait for it to actually be paused before
	// snapshotting. The pump checks the pause flag *before* the lock
	// acquisition for the next Feed, so once we observe `paused`, we
	// know the host mutex is free and Snapshot will not starve.
	pause.Store(true)
	select {
	case <-paused:
		dbg("[%s] pump paused", app.Name)
	case <-pumpDone:
		dbg("[%s] pump done (pty closed)", app.Name)
	case <-time.After(500 * time.Millisecond):
		dbg("[%s] pump pause timeout; dumping goroutines", app.Name)
		if os.Getenv("MUX_SPIKE_DEBUG") != "" {
			buf := make([]byte, 1<<20)
			n := stackAll(buf)
			fmt.Fprintf(os.Stderr, "%s\n", buf[:n])
		}
	}

	dbg("[%s] snapshotting", app.Name)
	snap := host.Snapshot()
	ansi := host.Render()
	dbg("[%s] snapshot+render complete", app.Name)
	// Release the pump so the deferred Close() drains cleanly.
	select {
	case <-resume: // already closed
	default:
		close(resume)
	}

	pumpMu.Lock()
	meta.BytesFromPTY, meta.FrameCount = bytesRead, frames
	pumpMu.Unlock()
	meta.WallDurationMs = time.Since(start).Milliseconds()
	meta.ExitCode = sessExitCode(sess)

	dbg("[%s] writing %d bytes from %d frames to %s", app.Name, meta.BytesFromPTY, meta.FrameCount, appDir)
	return render.Write(appDir, snap, ansi, meta)
}

func stackAll(buf []byte) int {
	return runtime.Stack(buf, true)
}

func dbg(format string, args ...interface{}) {
	if os.Getenv("MUX_SPIKE_DEBUG") == "" {
		return
	}
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// sleepOrDone returns true if d elapsed, false if done fired first.
func sleepOrDone(d time.Duration, done <-chan struct{}) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-done:
		return false
	}
}

// sessExitCode returns the child's exit code if it has exited, or -1 if it
// is still running. The corpus walk kills the child via Close() on the
// defer; the actual exit code is best-effort.
func sessExitCode(sess *pty.Session) int {
	if sess == nil || sess.Cmd == nil || sess.Cmd.ProcessState == nil {
		return -1
	}
	return sess.Cmd.ProcessState.ExitCode()
}

func splitCSV(s string) []string {
	var out []string
	cur := ""
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(s[i])
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
