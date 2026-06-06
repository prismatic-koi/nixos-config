// Per-cell diagnostic. Reads a cells.json written by `mux-spike corpus`
// and dumps the structured contents of one cell or a contiguous range.
package cmd

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/prismatic-koi/nixos-config/modules/programs/prism/mux-spike/internal/vt"
)

// Screenshot implements `mux-spike screenshot --in FILE --cell R,C`.
func Screenshot(args []string) error {
	fs := flag.NewFlagSet("screenshot", flag.ContinueOnError)
	in := fs.String("in", "", "path to a cells.json file written by `mux-spike corpus`")
	cellRC := fs.String("cell", "", "row,col (0-indexed)")
	cells := fs.Int("cells", 1, "number of consecutive cells in row to dump")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *in == "" || *cellRC == "" {
		return errors.New("usage: mux-spike screenshot --in FILE --cell R,C [--cells N]")
	}
	r, c, err := parseRC(*cellRC)
	if err != nil {
		return err
	}

	b, err := os.ReadFile(*in)
	if err != nil {
		return err
	}
	var snap vt.Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return fmt.Errorf("parse cells.json: %w", err)
	}

	if r < 0 || r >= snap.Rows {
		return fmt.Errorf("row %d out of bounds (rows=%d)", r, snap.Rows)
	}
	if c < 0 || c >= snap.Cols {
		return fmt.Errorf("col %d out of bounds (cols=%d)", c, snap.Cols)
	}
	end := c + *cells
	if end > snap.Cols {
		end = snap.Cols
	}

	fmt.Printf("cells.json %s — rows=%d cols=%d alt_screen=%v cursor=(%d,%d)\n",
		*in, snap.Rows, snap.Cols, snap.AltScreen, snap.CursorX, snap.CursorY)
	for col := c; col < end; col++ {
		cell := snap.Cells[r][col]
		fmt.Printf("(%d,%d) glyph=%q width=%d fg=%s/%06x bg=%s/%06x attrs=0x%02x underline=%d ul_color=%06x link=%q\n",
			r, col, cell.Glyph, cell.Width,
			cell.FgKind, cell.FgValue,
			cell.BgKind, cell.BgValue,
			cell.Attrs, cell.Underline, cell.ULColor,
			cell.Link,
		)
	}
	return nil
}

func parseRC(s string) (int, int, error) {
	parts := strings.SplitN(s, ",", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid --cell %q (want R,C)", s)
	}
	r, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid row %q: %w", parts[0], err)
	}
	c, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid col %q: %w", parts[1], err)
	}
	return r, c, nil
}
