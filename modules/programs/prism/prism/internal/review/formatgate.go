package review

// formatgate.go — formatter pre-flight gate for `prism review`.
//
// A five-agent LLM round is the wrong mechanism to discover a defect a
// deterministic tool reports for free in about a second — for example a
// missing newline at end of file, a plain `gofmt` violation. `pr-gate` CI
// blocks the merge on such a defect regardless, so a review round on it adds
// nothing CI does not already provide. This gate catches those defects first.
//
// This gate copies the shape of the pre-flight rebase gate in preflight.go:
// it runs before any review-agent session is spawned, and on refusal it
// exits non-zero and spawns no agents. Because it runs before RunAsync
// creates any per-agent session rows, a refusal here structurally cannot
// affect the review-cycle counter (NextRoundNumber counts those rows) —
// the same argument that applies to Preflight.
//
// Scope: Go (gofmt) and Nix (nixfmt --check) — the two formatters this repo
// enforces in AGENTS.md and pr-gate. Files are discovered via
// `git diff --name-only <remote>/<branch>...HEAD`, the same three-dot diff
// against the resolved base ref that Preflight has already fetched.
//
// Fail-open on a missing formatter binary is deliberate: a review that cannot
// run because a tool is absent is worse than a review that runs without the
// gate. Do not harden this into a hard failure.

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// FormatGateOpts configures the formatter gate.
type FormatGateOpts struct {
	// Worktree is the absolute path to the git worktree. Required.
	Worktree string
	// Remote is the git remote to diff against. Defaults to "origin".
	Remote string
	// Branch is the upstream branch to diff against. Defaults to "main".
	// Preflight has already fetched <Remote>/<Branch> by the time this gate
	// runs, so no additional fetch is performed here.
	Branch string
	// OnProgress is an optional callback invoked for progress events. When
	// nil, progress is silent.
	OnProgress func(line string)
	// gitRunner is an injectable runner for tests; nil = real git. Shares
	// the gitRunner interface declared in preflight.go.
	gitRunner gitRunner
	// cmdRunner is an injectable runner for the formatter binaries; nil =
	// real exec.
	cmdRunner formatCmdRunner
	// lookPath resolves a binary name to a path, mirroring exec.LookPath.
	// Injectable for tests; nil = exec.LookPath.
	lookPath func(name string) (string, error)
}

// formatCmdRunner is the test seam for executing a formatter binary in a
// specific worktree.
type formatCmdRunner interface {
	run(worktree, name string, args ...string) (stdout string, stderr string, exitCode int, err error)
}

// FormatGateError is returned by FormatGate when the gate refuses the
// review because one or more touched files fail their formatter check.
type FormatGateError struct {
	// Msg is the user-facing error message, ready to display.
	Msg string
}

func (e *FormatGateError) Error() string { return e.Msg }

// nixfmtNotFormattedRe matches nixfmt --check's "<path>: not formatted"
// output line, used to extract offending file paths precisely when nixfmt
// reports a clean "not formatted" verdict rather than a parse error.
var nixfmtNotFormattedRe = regexp.MustCompile(`^(\S+):\s+not formatted\s*$`)

// FormatGate runs the formatter pre-flight gate. It inspects the files
// changed relative to <Remote>/<Branch> (default origin/main) and runs
// `gofmt -l` against touched .go files and `nixfmt --check` against touched
// .nix files. It returns nil when there is nothing to check, or when
// everything checked is correctly formatted — the review may proceed. It
// returns a *FormatGateError naming the offending files and the exact fix
// command when a check fails. If a formatter binary is not on PATH, that
// language's check is skipped with a progress warning rather than blocking
// the review (fail-open — see the file-level comment).
func FormatGate(opts FormatGateOpts) error {
	if opts.Worktree == "" {
		return &FormatGateError{Msg: "prism review: formatgate: worktree path is required"}
	}
	remote := opts.Remote
	if remote == "" {
		remote = "origin"
	}
	branch := opts.Branch
	if branch == "" {
		branch = "main"
	}
	runner := opts.gitRunner
	if runner == nil {
		runner = realGit{}
	}
	cmdRunner := opts.cmdRunner
	if cmdRunner == nil {
		cmdRunner = execFormatCmd{}
	}
	lookPath := opts.lookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	progress := opts.OnProgress
	if progress == nil {
		progress = func(string) {}
	}

	remoteRef := remote + "/" + branch

	// Discover touched files via a three-dot diff against the resolved base
	// ref. Preflight has already fetched remoteRef before this gate runs, so
	// no fetch is performed here. --diff-filter=ACMR excludes deleted files
	// (nothing to format-check in a file that no longer exists) and renames
	// are included since the new path is what should be checked.
	out, stderr, code, err := runner.run(opts.Worktree, "diff", "--name-only", "--diff-filter=ACMR", remoteRef+"...HEAD")
	if err != nil || code != 0 {
		detail := strings.TrimSpace(stderr)
		if detail == "" && err != nil {
			detail = err.Error()
		}
		if detail == "" {
			detail = fmt.Sprintf("git diff exited with code %d", code)
		}
		return &FormatGateError{Msg: fmt.Sprintf("prism review: formatgate: could not list changed files: %s", detail)}
	}

	var goFiles, nixFiles []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch {
		case strings.HasSuffix(line, ".go"):
			goFiles = append(goFiles, line)
		case strings.HasSuffix(line, ".nix"):
			nixFiles = append(nixFiles, line)
		}
	}

	if len(goFiles) == 0 && len(nixFiles) == 0 {
		// No Go or Nix files touched — nothing for this gate to check.
		return nil
	}

	var offendingGo, offendingNix []string
	var diagnostics []string

	if len(goFiles) > 0 {
		if _, lpErr := lookPath("gofmt"); lpErr != nil {
			progress("formatgate: gofmt not found on PATH — skipping Go format check (fail-open)")
		} else {
			progress("formatgate: checking Go formatting …")
			stdout, stderr, code, runErr := cmdRunner.run(opts.Worktree, "gofmt", append([]string{"-l"}, goFiles...)...)
			switch {
			case runErr != nil:
				progress(fmt.Sprintf("formatgate: gofmt invocation failed (%v) — skipping Go format check (fail-open)", runErr))
			case code == 0:
				for _, line := range strings.Split(stdout, "\n") {
					line = strings.TrimSpace(line)
					if line != "" {
						offendingGo = append(offendingGo, line)
					}
				}
			default:
				// gofmt exited non-zero for a reason other than "files need
				// formatting" (e.g. a parse error) — the binary ran, so this
				// is not the fail-open "binary unavailable" case. Refuse and
				// surface the diagnostic; treat all checked files as
				// offending since gofmt did not identify a clean subset.
				offendingGo = append(offendingGo, goFiles...)
				detail := strings.TrimSpace(stderr)
				if detail == "" {
					detail = strings.TrimSpace(stdout)
				}
				if detail != "" {
					diagnostics = append(diagnostics, "gofmt: "+detail)
				}
			}
		}
	}

	if len(nixFiles) > 0 {
		if _, lpErr := lookPath("nixfmt"); lpErr != nil {
			progress("formatgate: nixfmt not found on PATH — skipping Nix format check (fail-open)")
		} else {
			progress("formatgate: checking Nix formatting …")
			stdout, stderr, code, runErr := cmdRunner.run(opts.Worktree, "nixfmt", append([]string{"--check"}, nixFiles...)...)
			switch {
			case runErr != nil:
				progress(fmt.Sprintf("formatgate: nixfmt invocation failed (%v) — skipping Nix format check (fail-open)", runErr))
			case code == 0:
				// All formatted.
			default:
				matched := false
				for _, line := range strings.Split(stdout, "\n") {
					line = strings.TrimSpace(line)
					if line == "" {
						continue
					}
					if m := nixfmtNotFormattedRe.FindStringSubmatch(line); m != nil {
						offendingNix = append(offendingNix, m[1])
						matched = true
					}
				}
				if !matched {
					// nixfmt reported something other than a clean
					// "not formatted" line (e.g. a parse error) — treat all
					// checked files as offending and surface the raw output.
					offendingNix = append(offendingNix, nixFiles...)
					detail := strings.TrimSpace(stdout)
					if detail == "" {
						detail = strings.TrimSpace(stderr)
					}
					if detail != "" {
						diagnostics = append(diagnostics, "nixfmt: "+detail)
					}
				}
			}
		}
	}

	if len(offendingGo) == 0 && len(offendingNix) == 0 {
		return nil
	}

	var b strings.Builder
	b.WriteString("prism review: formatting check failed — refusing to spawn review agents\n\n")
	if len(offendingGo) > 0 {
		fmt.Fprintf(&b, "Go files not gofmt-clean:\n")
		for _, f := range offendingGo {
			fmt.Fprintf(&b, "  %s\n", f)
		}
		fmt.Fprintf(&b, "\nFix with:\n\n    gofmt -w %s\n\n", strings.Join(offendingGo, " "))
	}
	if len(offendingNix) > 0 {
		fmt.Fprintf(&b, "Nix files not nixfmt-clean:\n")
		for _, f := range offendingNix {
			fmt.Fprintf(&b, "  %s\n", f)
		}
		fmt.Fprintf(&b, "\nFix with:\n\n    nixfmt %s\n\n", strings.Join(offendingNix, " "))
	}
	for _, d := range diagnostics {
		fmt.Fprintf(&b, "%s\n", d)
	}
	b.WriteString("\nThen commit, push, and re-run 'prism review <pr>'.")

	return &FormatGateError{Msg: b.String()}
}

// execFormatCmd shells out to the system formatter binaries.
type execFormatCmd struct{}

func (execFormatCmd) run(worktree, name string, args ...string) (string, string, int, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = worktree
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
			// Treat non-zero exit as a structured outcome rather than an
			// error — callers inspect exitCode for the formatter's pass/fail
			// signal.
			err = nil
		}
	}
	return stdout.String(), stderr.String(), exitCode, err
}
