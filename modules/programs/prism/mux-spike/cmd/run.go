// Interactive smoke test. Hosts one TUI under x/vt, forwards keystrokes
// from the user's terminal, paints the engine's view back to stdout.
//
// This is the AC's interactive smoke test: "the user can interactively edit,
// save with :w, and exit with :q without visible cell corruption". It is
// also the most useful test surface during development of the spike itself.
package cmd

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	xterm "github.com/charmbracelet/x/term"

	"github.com/prismatic-koi/nixos-config/modules/programs/prism/mux-spike/internal/pty"
	"github.com/prismatic-koi/nixos-config/modules/programs/prism/mux-spike/internal/vt"
)

// Run implements `mux-spike run <cmd> [args...]`.
func Run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cols := fs.Int("cols", 0, "PTY width; 0 means inherit from host terminal")
	rows := fs.Int("rows", 0, "PTY height; 0 means inherit from host terminal")
	if err := fs.Parse(args); err != nil {
		return err
	}
	argv := fs.Args()
	if len(argv) == 0 {
		return errors.New("usage: mux-spike run <cmd> [args...]")
	}

	// Inherit host terminal size if not overridden.
	if *cols == 0 || *rows == 0 {
		w, h, err := xterm.GetSize(uintptr(os.Stdout.Fd()))
		if err != nil || w <= 0 || h <= 0 {
			w, h = 120, 40
		}
		if *cols == 0 {
			*cols = w
		}
		if *rows == 0 {
			*rows = h
		}
	}

	// Put host stdin into raw mode so keystrokes flow through unbuffered.
	stdinFd := uintptr(os.Stdin.Fd())
	var restore func() error
	if xterm.IsTerminal(stdinFd) {
		st, err := xterm.MakeRaw(stdinFd)
		if err != nil {
			return fmt.Errorf("makeraw stdin: %w", err)
		}
		restore = func() error { return xterm.Restore(stdinFd, st) }
		defer func() { _ = restore() }()
	}

	// Switch host terminal into alt-screen so we don't pollute the scrollback
	// with the hosted TUI's frames, hide the cursor for the duration (paint
	// re-shows it per-frame at the engine's tracked position), and scrub
	// once with ED2 so any shell-prompt residue under the alt-screen layer
	// is gone before the first paint. Plain ANSI; no dep needed.
	os.Stdout.WriteString("\x1b[?1049h\x1b[?25l\x1b[2J\x1b[H")
	defer os.Stdout.WriteString("\x1b[?25h\x1b[?1049l")

	sess, err := pty.Start(argv, uint16(*cols), uint16(*rows))
	if err != nil {
		return fmt.Errorf("pty start: %w", err)
	}
	defer sess.Close()

	host := vt.New(*cols, *rows)

	// Drain emulator-→-PTY responses (DSR replies, mode queries). Without
	// this the hosted TUI deadlocks once its query responses pile up;
	// see vt.Host.DrainResponses.
	go func() { _ = host.DrainResponses(sess.File) }()

	// SIGWINCH: forward host terminal resize into PTY + emulator.
	winchCh := make(chan os.Signal, 1)
	signal.Notify(winchCh, syscall.SIGWINCH)
	defer signal.Stop(winchCh)

	var wg sync.WaitGroup
	done := make(chan struct{})

	// PTY → emulator pump.
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = host.FeedReader(sess.File)
		close(done)
	}()

	// stdin → PTY pump.
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				// Ctrl-q (0x11) is the host hotkey — exit the spike.
				for i := 0; i < n; i++ {
					if buf[i] == 0x11 {
						_ = sess.Close()
						return
					}
				}
				if _, werr := sess.File.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				if err == io.EOF {
					return
				}
				return
			}
		}
	}()

	// Resize pump.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-winchCh:
				w, h, err := xterm.GetSize(uintptr(os.Stdout.Fd()))
				if err != nil || w <= 0 || h <= 0 {
					continue
				}
				_ = sess.Resize(uint16(w), uint16(h))
				host.Resize(w, h)
			case <-done:
				return
			}
		}
	}()

	// Render pump — repaint at ~30 Hz. We could be cleverer (Touched()), but
	// for a smoke test 30 Hz is fine and the code stays tiny.
	ticker := time.NewTicker(33 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			// Drain pumps. The stdin reader is blocked on the host TTY;
			// give it a beat to notice via os.Stdin close on terminal
			// restore, then move on. Don't wait forever.
			waitCh := make(chan struct{})
			go func() { wg.Wait(); close(waitCh) }()
			select {
			case <-waitCh:
			case <-time.After(200 * time.Millisecond):
			}
			return nil
		case <-ticker.C:
			paint(host)
		}
	}
}

// paint writes one frame of the hosted TUI to stdout. Factored as
// build-then-write so the frame-building logic is testable without an
// attached TTY — see cmd/run_test.go.
func paint(host *vt.Host) {
	_, _ = os.Stdout.WriteString(buildFrame(host))
}

// buildFrame produces one frame of host-terminal output for the current
// emulator state.
//
// Why per-row absolute positioning rather than `\x1b[H` + host.Render():
//
//  1. x/vt's Render() emits rows separated by bare LF (no CR). In raw
//     mode — which run.go has set on stdin via xterm.MakeRaw — LF is
//     line-feed only: the cursor moves down one row at the current
//     column. Every row past the first drifts rightward by the previous
//     row's width. The first broken-render screenshot shows nvim's
//     tildes cascading to the right edge for exactly this reason.
//
//  2. Render() does NOT pad short rows to the emulator width. Cells
//     left unfilled by the engine carry over from whatever was last
//     painted into those cells on the host terminal. The third
//     screenshot shows two complete nvim status bars at different y
//     positions — the previous frame's bottom row was still painted
//     when the new frame's shorter rows landed.
//
// Per-row absolute positioning sidesteps both: every row gets
// `\x1b[<y+1>;1H` so column drift is impossible, and a trailing
// `\x1b[0m\x1b[K` erases anything left of the previous frame on that
// row's tail. Cursor is hidden across the frame and reshown at the
// engine's tracked position (mirroring DECTCEM) at the very end.
func buildFrame(host *vt.Host) string {
	snap := host.Snapshot()
	rows := host.RenderRows()

	var b strings.Builder
	// Hide cursor while we walk it row-to-row so users don't see it dart
	// around mid-paint at 30 Hz.
	b.WriteString("\x1b[?25l")
	for y, row := range rows {
		// CSI row;col H — absolute position, 1-indexed.
		fmt.Fprintf(&b, "\x1b[%d;1H", y+1)
		b.WriteString(row)
		// Reset SGR before EL so the erase uses the default background,
		// not whatever colour the last cell in this row left active.
		b.WriteString("\x1b[0m\x1b[K")
	}
	// Position the host cursor where the engine thinks it is. Snapshot's
	// CursorX/Y are 0-indexed; CSI H wants 1-indexed.
	fmt.Fprintf(&b, "\x1b[%d;%dH", snap.CursorY+1, snap.CursorX+1)
	// Mirror the engine's DECTCEM state. If the hosted TUI hid its
	// cursor (nvim in some modes, htop) we leave it hidden; otherwise we
	// re-show so users get the standard blinking cursor at the right cell.
	if host.CursorVisible() {
		b.WriteString("\x1b[?25h")
	}
	return b.String()
}
