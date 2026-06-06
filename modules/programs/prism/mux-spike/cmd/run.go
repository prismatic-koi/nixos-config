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
	// with the hosted TUI's frames. Plain ANSI; no dep needed.
	os.Stdout.WriteString("\x1b[?1049h\x1b[?25l")
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

// paint clears the host terminal and writes the emulator's rendering to
// stdout. The CSI sequences here are deliberately bare-bones — anything
// fancier (sync output, double-buffering) would muddy the fidelity signal.
func paint(host *vt.Host) {
	// Move cursor home, then write the rendered cells. No clear — the
	// engine's Render() emits a full repaint per row, which is sufficient.
	var b strings.Builder
	b.WriteString("\x1b[H")
	b.WriteString(host.Render())
	_, _ = os.Stdout.WriteString(b.String())
}
