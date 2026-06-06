# mux-spike

Spike binary for issue #2141 — characterise `charmbracelet/x/vt` fidelity
against a hand-graded TUI corpus before committing to the prism-native
multiplexer programme proposed in
`modules/programs/prism/prism/docs/multiplexer-proposal.md`.

This is **not production code**. It is an interactive characterisation tool.
Its surviving artefacts after a verdict are `reports/report.md` and (on a
proceed-verdict) the curated `corpus/corpus.toml`. Everything else is
disposable — see the issue's "Reversibility" AC.

Three subcommands:

- `mux-spike run <cmd...>` — interactive smoke test, hosts one TUI under
  `x/vt` and forwards keystrokes from your terminal. Quit with `Ctrl-q`.
- `mux-spike corpus --out <dir>` — non-interactive walk of `corpus/corpus.toml`,
  capturing each app's VT state to `<dir>/<app>/`.
- `mux-spike screenshot --in <dir>/<app>/cells.json --cell r,c [--cells n]`
  — per-cell diagnostic, hex-dumps the VT engine's view of the requested
  cell(s).

The tmux apples-to-apples capture comes from
`corpus/capture-via-tmux.sh --out <dir>`.
