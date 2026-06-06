# sidebar-spike

Non-functional UI mockup of the herdr-shape sidebar planned for the
prism-native multiplexer programme. Tracking parent: issue #2147.
Spike issue: #2148.

This is **not production code**. It is an interactive visual mockup
whose surviving artefact is the design-doc addendum to
`modules/programs/prism/prism/docs/multiplexer-proposal.md` (added on
the "ship it" verdict). The spike directory itself is deleted in a
follow-up cleanup PR after the codified design has been consumed by
planned PR #3 of the multiplexer programme.

## Run it

```bash
nix run .#sidebar-spike
nix run .#sidebar-spike -- --fixture minimal     # smaller scratch fixture
```

Interactive keys:

| Key | Action |
|---|---|
| `↑` / `↓` (or `k` / `j`) | Move selection within the tree |
| `←` / `→` (or `h` / `l`) | Collapse / expand repo groups |
| `Enter` | Select a session (right pane reflects current selection) |
| `Tab` / `Shift-Tab` | Cycle the selected session's pane (agent/term/edit) |
| `q` or `Ctrl-C` | Quit |

## What's mocked

- **PTYs** — none. The right pane is a static placeholder describing
  the selected session.
- **Session data** — `internal/mockdata` holds the fixtures.
- **Bus subscription** — a 200 ms wall-clock ticker drives the
  scripted state transitions from the fixture.

What is real: the bubbletea render loop, keyboard input handling, and
the state-to-glyph / state-to-colour mapping under refinement.

## What lives where

| Path | What |
|---|---|
| `main.go` | bubbletea root model + animation engine |
| `internal/sidebar/style.go` | glyph + colour mapping (the iteration target) |
| `internal/sidebar/sidebar.go` | flatten + render the tree |
| `internal/model/` | mock session-tree types |
| `internal/mockdata/` | fixtures + scripted transition timelines |
| `design-notes.md` | working notes captured during iteration |

The visual vocabulary — glyphs, colours, padding — is concentrated in
`internal/sidebar/style.go` precisely so iteration revisions don't
sprawl across the renderer.

## How this connects to the multiplexer programme

The umbrella tracking issue is #2147. The 12-PR programme breakdown
lives there. This spike is filed as #2148 and exists to feed PR #3
(`internal/mux/render/`) a codified visual reference rather than
asking the worker on that PR to invent the design.
