// Package corpus loads the TOML manifest and provides a keystroke-shorthand
// expander shared by the Go driver and the tmux comparison script.
package corpus

import (
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// Manifest is the root of corpus.toml.
type Manifest struct {
	Apps []App `toml:"apps"`
}

// App is one entry in the corpus.
type App struct {
	Name                string   `toml:"name"`
	Argv                []string `toml:"argv"`
	Cols                int      `toml:"cols"`
	Rows                int      `toml:"rows"`
	SettleMs            int      `toml:"settle_ms"`
	Triggers            []string `toml:"triggers"`
	PostTriggerSettleMs int      `toml:"post_trigger_settle_ms"`
	Notes               string   `toml:"notes"`
}

// Load reads and parses corpus.toml from path.
func Load(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := toml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(m.Apps) == 0 {
		return nil, fmt.Errorf("%s: no [[apps]] entries", path)
	}
	return &m, nil
}

// ExpandKeystroke converts one manifest-shorthand string ("hello",
// "<esc>:wq<cr>", "<c-w>j") into the byte sequence to write to the PTY.
//
// Recognised tokens (case-insensitive, angle-bracketed):
//
//	<esc> <cr> <lf> <tab> <bs> <space>
//	<up> <down> <left> <right>
//	<f1>..<f12>
//	<c-X>  where X is a letter (Ctrl-A through Ctrl-Z)
//
// Anything outside <...> is forwarded verbatim. Unknown tokens are written
// literally so the harness fails loudly rather than silently dropping input.
func ExpandKeystroke(s string) []byte {
	var out []byte
	i := 0
	for i < len(s) {
		c := s[i]
		if c != '<' {
			out = append(out, c)
			i++
			continue
		}
		// Look for closing '>'.
		end := strings.IndexByte(s[i:], '>')
		if end < 0 {
			out = append(out, c)
			i++
			continue
		}
		tok := s[i : i+end+1]
		bs := expandToken(tok)
		out = append(out, bs...)
		i += end + 1
	}
	return out
}

func expandToken(tok string) []byte {
	lower := strings.ToLower(tok)
	switch lower {
	case "<esc>":
		return []byte{0x1b}
	case "<cr>":
		return []byte{0x0d}
	case "<lf>":
		return []byte{0x0a}
	case "<tab>":
		return []byte{0x09}
	case "<bs>":
		return []byte{0x7f}
	case "<space>":
		return []byte{0x20}
	case "<up>":
		return []byte("\x1b[A")
	case "<down>":
		return []byte("\x1b[B")
	case "<right>":
		return []byte("\x1b[C")
	case "<left>":
		return []byte("\x1b[D")
	case "<f1>":
		return []byte("\x1bOP")
	case "<f2>":
		return []byte("\x1bOQ")
	case "<f3>":
		return []byte("\x1bOR")
	case "<f4>":
		return []byte("\x1bOS")
	case "<f5>":
		return []byte("\x1b[15~")
	case "<f6>":
		return []byte("\x1b[17~")
	case "<f7>":
		return []byte("\x1b[18~")
	case "<f8>":
		return []byte("\x1b[19~")
	case "<f9>":
		return []byte("\x1b[20~")
	case "<f10>":
		return []byte("\x1b[21~")
	case "<f11>":
		return []byte("\x1b[23~")
	case "<f12>":
		return []byte("\x1b[24~")
	}
	// <c-X> — control sequence.
	if len(lower) == 5 && strings.HasPrefix(lower, "<c-") && lower[4] == '>' {
		ch := lower[3]
		if ch >= 'a' && ch <= 'z' {
			return []byte{byte(ch - 'a' + 1)}
		}
	}
	// Unknown token — write literally so the operator can spot it.
	return []byte(tok)
}
