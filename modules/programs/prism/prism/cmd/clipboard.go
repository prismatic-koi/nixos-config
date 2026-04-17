package cmd

// prism clipboard — host-side clipboard utilities for container-mode opencode.
//
// Subcommands:
//
//	paste-image   Read a PNG image from the host clipboard, stage it to
//	              ~/.cache/prism/clipboard/<uuid>.png, and print the path.
//	              Exits non-zero when the clipboard contains no image or an
//	              image format other than PNG; emits no output in that case.
//
//	clean         Remove stale files from ~/.cache/prism/clipboard/ (older
//	              than the configured TTL, default 1 hour). Idempotent — safe
//	              to run even when the directory does not exist.
//
// Design notes:
//   - All clipboard I/O is done on the host. No clipboard tool, socket, or
//     env var is exposed inside the running container.
//   - The staging directory (~/.cache/prism/clipboard/) is bind-mounted into
//     the container read-only so opencode's stat() call resolves without
//     modification. The container cannot write to it.
//   - Platform detection is done at runtime via WAYLAND_DISPLAY / DISPLAY
//     env vars on Linux, and runtime.GOOS on Darwin.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

// clipboardCacheDir returns the path to the clipboard staging directory.
func clipboardCacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("clipboard: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".cache", "prism", "clipboard"), nil
}

// clipboardStagingTTL is the age after which files in the clipboard staging
// directory are considered stale and eligible for cleanup.
const clipboardStagingTTL = time.Hour

// readClipboardPNG reads PNG image bytes from the host clipboard.
// Returns an error when the clipboard contains no image or a non-PNG format.
//
// Platform dispatch:
//   - Darwin: osascript reads NSPasteboard as PNG → temp file → bytes.
//   - Linux/Wayland (WAYLAND_DISPLAY set): wl-paste -t image/png.
//   - Linux/X11 (DISPLAY set, no WAYLAND_DISPLAY): xclip -selection clipboard -t image/png -o.
//   - Otherwise: returns an error.
func readClipboardPNG() ([]byte, error) {
	switch runtime.GOOS {
	case "darwin":
		return readClipboardPNGDarwin()
	case "linux":
		return readClipboardPNGLinux()
	default:
		return nil, fmt.Errorf("clipboard: unsupported platform %q", runtime.GOOS)
	}
}

// readClipboardPNGLinux reads PNG image bytes from the host clipboard on Linux.
// Tries Wayland first (WAYLAND_DISPLAY), then X11 (DISPLAY).
func readClipboardPNGLinux() ([]byte, error) {
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		// Wayland: wl-paste -t image/png exits non-zero when the clipboard
		// contains no image or a different MIME type.
		out, err := exec.Command("wl-paste", "-t", "image/png").Output()
		if err != nil {
			return nil, fmt.Errorf("clipboard: no PNG image in Wayland clipboard (wl-paste: %w)", err)
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("clipboard: wl-paste returned empty output")
		}
		if !isPNG(out) {
			return nil, fmt.Errorf("clipboard: clipboard data is not a PNG image")
		}
		return out, nil
	}

	if os.Getenv("DISPLAY") != "" {
		// X11: xclip exits non-zero when the clipboard target type is unavailable.
		out, err := exec.Command("xclip", "-selection", "clipboard", "-t", "image/png", "-o").Output()
		if err != nil {
			return nil, fmt.Errorf("clipboard: no PNG image in X11 clipboard (xclip: %w)", err)
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("clipboard: xclip returned empty output")
		}
		if !isPNG(out) {
			return nil, fmt.Errorf("clipboard: clipboard data is not a PNG image")
		}
		return out, nil
	}

	return nil, fmt.Errorf("clipboard: neither WAYLAND_DISPLAY nor DISPLAY is set — cannot access host clipboard")
}

// readClipboardPNGDarwin reads PNG image bytes from the macOS NSPasteboard.
// Uses osascript to write the pasteboard contents to a temp file as PNG, then
// reads and returns the bytes. Returns an error when the pasteboard contains
// no image or the image cannot be written as PNG.
func readClipboardPNGDarwin() ([]byte, error) {
	// Write a temp file path for osascript to use as the output destination.
	tmpFile, err := os.CreateTemp("", "prism-clipboard-*.png")
	if err != nil {
		return nil, fmt.Errorf("clipboard: create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)

	// osascript: read NSPasteboard as PNG and write to the temp file.
	// If the pasteboard has no image, `theImage` will be missing data and
	// the write will fail with an error, causing osascript to exit non-zero.
	script := fmt.Sprintf(`
set tmpPath to %q
try
    set theImage to (the clipboard as «class PNGf»)
on error errMsg
    error "No PNG image in clipboard: " & errMsg
end try
set fileRef to open for access (POSIX file tmpPath) with write permission
set eof of fileRef to 0
write theImage to fileRef
close access fileRef
`, tmpPath)

	out, err := exec.Command("osascript", "-e", script).CombinedOutput()
	if err != nil {
		// Remove the temp file (it may be empty or partial) so cleanup is clean.
		_ = os.Remove(tmpPath)
		return nil, fmt.Errorf("clipboard: osascript failed (no PNG in pasteboard?): %w — %s", err, string(out))
	}

	data, err := os.ReadFile(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("clipboard: read osascript output: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("clipboard: osascript wrote empty file — clipboard may contain no image")
	}
	if !isPNG(data) {
		return nil, fmt.Errorf("clipboard: clipboard data is not a PNG image")
	}
	return data, nil
}

// isPNG returns true when data begins with the PNG file magic bytes.
// The PNG signature is: 0x89 0x50 0x4E 0x47 0x0D 0x0A 0x1A 0x0A
func isPNG(data []byte) bool {
	if len(data) < 8 {
		return false
	}
	return data[0] == 0x89 &&
		data[1] == 'P' &&
		data[2] == 'N' &&
		data[3] == 'G' &&
		data[4] == 0x0D &&
		data[5] == 0x0A &&
		data[6] == 0x1A &&
		data[7] == 0x0A
}

// runClipboardPasteImage is the implementation of `prism clipboard paste-image`.
// It reads a PNG from the host clipboard, stages it to the clipboard cache dir,
// and prints the absolute path on stdout. Exits non-zero silently when there is
// no image (or a non-PNG image) in the clipboard.
func runClipboardPasteImage(cmd *cobra.Command, args []string) error {
	data, err := readClipboardPNG()
	if err != nil {
		// Exit non-zero but emit no output so the tmux keybind can detect
		// failure and fall through without injecting a spurious bracketed-paste.
		return fmt.Errorf("%w", err)
	}

	cacheDir, err := clipboardCacheDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return fmt.Errorf("clipboard: create staging dir %q: %w", cacheDir, err)
	}

	id := uuid.New().String()
	stagingPath := filepath.Join(cacheDir, id+".png")
	if err := os.WriteFile(stagingPath, data, 0o644); err != nil {
		return fmt.Errorf("clipboard: write staged image %q: %w", stagingPath, err)
	}

	fmt.Println(stagingPath)
	return nil
}

// runClipboardClean removes stale files from the clipboard staging directory.
// Files older than clipboardStagingTTL are removed. Directories (unexpected)
// are left in place. Errors on individual files are logged to stderr but do not
// cause the command to exit non-zero.
func runClipboardClean(cmd *cobra.Command, args []string) error {
	cacheDir, err := clipboardCacheDir()
	if err != nil {
		return err
	}

	entries, err := os.ReadDir(cacheDir)
	if os.IsNotExist(err) {
		// Directory doesn't exist yet — nothing to clean.
		return nil
	}
	if err != nil {
		return fmt.Errorf("clipboard clean: read dir %q: %w", cacheDir, err)
	}

	cutoff := time.Now().Add(-clipboardStagingTTL)
	cleaned := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			fmt.Fprintf(os.Stderr, "clipboard clean: stat %q: %v\n", entry.Name(), err)
			continue
		}
		if info.ModTime().Before(cutoff) {
			path := filepath.Join(cacheDir, entry.Name())
			if err := os.Remove(path); err != nil {
				fmt.Fprintf(os.Stderr, "clipboard clean: remove %q: %v\n", path, err)
			} else {
				cleaned++
			}
		}
	}

	fmt.Printf("clipboard clean: removed %d stale file(s) from %s\n", cleaned, cacheDir)
	return nil
}

var clipboardCmd = &cobra.Command{
	Use:   "clipboard",
	Short: "Host-side clipboard utilities for container-mode opencode",
}

var clipboardPasteImageCmd = &cobra.Command{
	Use:   "paste-image",
	Short: "Read a PNG from the host clipboard, stage it, and print the path",
	Long: `Read a PNG image from the host clipboard, write it to
~/.cache/prism/clipboard/<uuid>.png, and print the absolute path on stdout.

Exits non-zero (with no output) when the clipboard contains no image or an
image format other than PNG, so the tmux keybind can detect failure and fall
through to default behaviour without injecting a spurious bracketed-paste.

Platform support:
  - Linux/Wayland: wl-paste -t image/png
  - Linux/X11: xclip -selection clipboard -t image/png -o
  - macOS: osascript reading NSPasteboard as PNG`,
	RunE:         runClipboardPasteImage,
	SilenceUsage: true,
}

var clipboardCleanCmd = &cobra.Command{
	Use:   "clean",
	Short: "Remove stale files from the clipboard staging directory",
	Long: `Remove files older than 1 hour from ~/.cache/prism/clipboard/.

Idempotent — safe to run even when the directory does not exist.
Run by prism daemon startup and session-end hooks to prevent unbounded growth.`,
	RunE: runClipboardClean,
}

func init() {
	clipboardCmd.AddCommand(clipboardPasteImageCmd)
	clipboardCmd.AddCommand(clipboardCleanCmd)
	rootCmd.AddCommand(clipboardCmd)
}
