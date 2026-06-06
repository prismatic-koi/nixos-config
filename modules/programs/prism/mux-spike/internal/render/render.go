// Package render serialises VT snapshots to on-disk capture artefacts.
//
// Four artefacts per app per run:
//   - cells.txt   plain-text grid (rows x cols of glyphs, newline per row)
//   - cells.ansi  the engine's own ANSI re-emission (Emulator.Render())
//   - cells.json  structured per-cell dump for programmatic comparison
//   - meta.json   wall-clock, exit code, frame count, panics
//
// These mirror the issue's artefact contract verbatim.
package render

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/prismatic-koi/nixos-config/modules/programs/prism/mux-spike/internal/vt"
)

// Meta is the per-app diagnostic record written next to the captures.
type Meta struct {
	App                 string `json:"app"`
	Cols                int    `json:"cols"`
	Rows                int    `json:"rows"`
	WallDurationMs      int64  `json:"wall_duration_ms"`
	ExitCode            int    `json:"exit_code"`
	FrameCount          int    `json:"frame_count"`
	PanicCaught         string `json:"panic_caught,omitempty"`
	PanicStack          string `json:"panic_stack,omitempty"`
	TriggersSent        int    `json:"triggers_sent"`
	BytesFromPTY        int64  `json:"bytes_from_pty"`
	SettleMs            int    `json:"settle_ms"`
	PostTriggerSettleMs int    `json:"post_trigger_settle_ms"`
	Notes               string `json:"notes,omitempty"`
}

// Write writes the four capture artefacts under dir, which it creates if
// necessary.
func Write(dir string, snap vt.Snapshot, ansi string, meta Meta) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	if err := writeCellsText(filepath.Join(dir, "cells.txt"), snap); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "cells.ansi"), []byte(ansi), 0o644); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, "cells.json"), snap); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, "meta.json"), meta); err != nil {
		return err
	}
	return nil
}

// writeCellsText writes the visible glyph grid as plain text, one line per
// row, glyph-only. This is the cheap diff target — `diff -u` against the
// tmux capture's text view will surface the most obvious disagreement.
func writeCellsText(path string, snap vt.Snapshot) error {
	var out strings.Builder
	var row strings.Builder
	for _, r := range snap.Cells {
		row.Reset()
		for _, cell := range r {
			row.WriteString(cell.Glyph)
		}
		// Trim trailing spaces — tmux capture-pane also trims by default,
		// keeping the two outputs comparable.
		out.WriteString(strings.TrimRight(row.String(), " "))
		out.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(out.String()), 0o644)
}

func writeJSON(path string, v interface{}) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
