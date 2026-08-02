package review

import (
	"errors"
	"strings"
	"testing"
)

// ── fake formatCmdRunner: scripted responses keyed by "name args..." ─────────

type fakeFormatCmd struct {
	script map[string]scriptedResponse
	calls  []string
}

func newFakeFormatCmd() *fakeFormatCmd {
	return &fakeFormatCmd{script: map[string]scriptedResponse{}}
}

func (f *fakeFormatCmd) on(key string, resp scriptedResponse) *fakeFormatCmd {
	f.script[key] = resp
	return f
}

func (f *fakeFormatCmd) run(_ string, name string, args ...string) (string, string, int, error) {
	key := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, key)
	resp, ok := f.script[key]
	if !ok {
		return "", "", 1, errors.New("fakeFormatCmd: no script for: " + key)
	}
	return resp.stdout, resp.stderr, resp.exitCode, resp.err
}

// fakeLookPath returns a lookPath func that reports the named binaries as
// present; anything else is reported missing.
func fakeLookPath(present ...string) func(string) (string, error) {
	set := map[string]bool{}
	for _, p := range present {
		set[p] = true
	}
	return func(name string) (string, error) {
		if set[name] {
			return "/usr/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
}

// ── tests ──────────────────────────────────────────────────────────────────

// TestFormatGate_NoGoOrNixFilesSkips verifies that a diff touching neither Go
// nor Nix files skips the gate entirely — no formatter is invoked.
func TestFormatGate_NoGoOrNixFilesSkips(t *testing.T) {
	fg := newFakeGit().
		on("diff --name-only --diff-filter=ACMR origin/main...HEAD", scriptedResponse{
			stdout: "README.md\ndocs/notes.txt\n", exitCode: 0,
		})
	fc := newFakeFormatCmd()

	err := FormatGate(FormatGateOpts{
		Worktree:  "/fake/worktree",
		gitRunner: fg,
		cmdRunner: fc,
		lookPath:  fakeLookPath("gofmt", "nixfmt"),
	})
	if err != nil {
		t.Fatalf("FormatGate: unexpected error: %v", err)
	}
	if len(fc.calls) != 0 {
		t.Errorf("FormatGate: expected no formatter invocations; got %v", fc.calls)
	}
}

// TestFormatGate_CleanGoAndNixProceeds verifies that when gofmt -l and
// nixfmt --check both report no issues, the gate returns nil.
func TestFormatGate_CleanGoAndNixProceeds(t *testing.T) {
	fg := newFakeGit().
		on("diff --name-only --diff-filter=ACMR origin/main...HEAD", scriptedResponse{
			stdout: "main.go\nmodule.nix\n", exitCode: 0,
		})
	fc := newFakeFormatCmd().
		on("gofmt -l main.go", scriptedResponse{stdout: "", exitCode: 0}).
		on("nixfmt --check module.nix", scriptedResponse{stdout: "", exitCode: 0})

	err := FormatGate(FormatGateOpts{
		Worktree:  "/fake/worktree",
		gitRunner: fg,
		cmdRunner: fc,
		lookPath:  fakeLookPath("gofmt", "nixfmt"),
	})
	if err != nil {
		t.Fatalf("FormatGate: unexpected error: %v", err)
	}
}

// TestFormatGate_DirtyGoRefuses verifies that a Go file gofmt flags as
// needing formatting causes a refusal naming the file and the fix command.
func TestFormatGate_DirtyGoRefuses(t *testing.T) {
	fg := newFakeGit().
		on("diff --name-only --diff-filter=ACMR origin/main...HEAD", scriptedResponse{
			stdout: "internal/foo/bar.go\n", exitCode: 0,
		})
	fc := newFakeFormatCmd().
		on("gofmt -l internal/foo/bar.go", scriptedResponse{stdout: "internal/foo/bar.go\n", exitCode: 0})

	err := FormatGate(FormatGateOpts{
		Worktree:  "/fake/worktree",
		gitRunner: fg,
		cmdRunner: fc,
		lookPath:  fakeLookPath("gofmt"),
	})
	if err == nil {
		t.Fatal("FormatGate: expected refusal for dirty Go file, got nil")
	}
	var fe *FormatGateError
	if !errors.As(err, &fe) {
		t.Fatalf("FormatGate: error is not *FormatGateError: %T %v", err, err)
	}
	for _, want := range []string{
		"internal/foo/bar.go",
		"gofmt -w internal/foo/bar.go",
	} {
		if !strings.Contains(fe.Msg, want) {
			t.Errorf("FormatGate: refusal message missing %q; got:\n%s", want, fe.Msg)
		}
	}
}

// TestFormatGate_DirtyNixRefuses verifies that a Nix file nixfmt --check
// flags as unformatted causes a refusal naming the file and the fix command.
func TestFormatGate_DirtyNixRefuses(t *testing.T) {
	fg := newFakeGit().
		on("diff --name-only --diff-filter=ACMR origin/main...HEAD", scriptedResponse{
			stdout: "modules/foo.nix\n", exitCode: 0,
		})
	fc := newFakeFormatCmd().
		on("nixfmt --check modules/foo.nix", scriptedResponse{
			stdout: "modules/foo.nix: not formatted\n", exitCode: 1,
		})

	err := FormatGate(FormatGateOpts{
		Worktree:  "/fake/worktree",
		gitRunner: fg,
		cmdRunner: fc,
		lookPath:  fakeLookPath("nixfmt"),
	})
	if err == nil {
		t.Fatal("FormatGate: expected refusal for dirty Nix file, got nil")
	}
	var fe *FormatGateError
	if !errors.As(err, &fe) {
		t.Fatalf("FormatGate: error is not *FormatGateError: %T %v", err, err)
	}
	for _, want := range []string{
		"modules/foo.nix",
		"nixfmt modules/foo.nix",
	} {
		if !strings.Contains(fe.Msg, want) {
			t.Errorf("FormatGate: refusal message missing %q; got:\n%s", want, fe.Msg)
		}
	}
}

// TestFormatGate_MissingGofmtSkipsFailOpen verifies that when gofmt is not on
// PATH, the Go check is skipped (fail-open) rather than blocking the review.
func TestFormatGate_MissingGofmtSkipsFailOpen(t *testing.T) {
	fg := newFakeGit().
		on("diff --name-only --diff-filter=ACMR origin/main...HEAD", scriptedResponse{
			stdout: "main.go\n", exitCode: 0,
		})
	fc := newFakeFormatCmd() // no script entries — must not be called

	var progressLines []string
	err := FormatGate(FormatGateOpts{
		Worktree:   "/fake/worktree",
		gitRunner:  fg,
		cmdRunner:  fc,
		lookPath:   fakeLookPath(), // nothing present
		OnProgress: func(l string) { progressLines = append(progressLines, l) },
	})
	if err != nil {
		t.Fatalf("FormatGate: expected fail-open (nil error) when gofmt missing; got %v", err)
	}
	if len(fc.calls) != 0 {
		t.Errorf("FormatGate: gofmt must not be invoked when missing from PATH; calls=%v", fc.calls)
	}
	found := false
	for _, l := range progressLines {
		if strings.Contains(l, "gofmt not found") {
			found = true
		}
	}
	if !found {
		t.Errorf("FormatGate: expected a progress warning about missing gofmt; got %v", progressLines)
	}
}

// TestFormatGate_MissingNixfmtSkipsFailOpen mirrors the gofmt case for nixfmt.
func TestFormatGate_MissingNixfmtSkipsFailOpen(t *testing.T) {
	fg := newFakeGit().
		on("diff --name-only --diff-filter=ACMR origin/main...HEAD", scriptedResponse{
			stdout: "module.nix\n", exitCode: 0,
		})
	fc := newFakeFormatCmd()

	err := FormatGate(FormatGateOpts{
		Worktree:  "/fake/worktree",
		gitRunner: fg,
		cmdRunner: fc,
		lookPath:  fakeLookPath(),
	})
	if err != nil {
		t.Fatalf("FormatGate: expected fail-open (nil error) when nixfmt missing; got %v", err)
	}
	if len(fc.calls) != 0 {
		t.Errorf("FormatGate: nixfmt must not be invoked when missing from PATH; calls=%v", fc.calls)
	}
}

// TestFormatGate_MixedCleanGoDirtyNix verifies that a mixed PR (clean Go,
// dirty Nix) refuses and names only the offending Nix file, not the clean
// Go file.
func TestFormatGate_MixedCleanGoDirtyNix(t *testing.T) {
	fg := newFakeGit().
		on("diff --name-only --diff-filter=ACMR origin/main...HEAD", scriptedResponse{
			stdout: "main.go\nmodule.nix\n", exitCode: 0,
		})
	fc := newFakeFormatCmd().
		on("gofmt -l main.go", scriptedResponse{stdout: "", exitCode: 0}).
		on("nixfmt --check module.nix", scriptedResponse{stdout: "module.nix: not formatted\n", exitCode: 1})

	err := FormatGate(FormatGateOpts{
		Worktree:  "/fake/worktree",
		gitRunner: fg,
		cmdRunner: fc,
		lookPath:  fakeLookPath("gofmt", "nixfmt"),
	})
	if err == nil {
		t.Fatal("FormatGate: expected refusal for mixed clean-Go/dirty-Nix PR")
	}
	var fe *FormatGateError
	if !errors.As(err, &fe) {
		t.Fatalf("FormatGate: error is not *FormatGateError: %T %v", err, err)
	}
	if strings.Contains(fe.Msg, "Go files not gofmt-clean") {
		t.Errorf("FormatGate: refusal must not mention Go section when Go is clean; got:\n%s", fe.Msg)
	}
	if !strings.Contains(fe.Msg, "module.nix") {
		t.Errorf("FormatGate: refusal missing offending Nix file; got:\n%s", fe.Msg)
	}
}

// TestFormatGate_NonMainBaseUsesResolvedRef verifies the diff is computed
// against the resolved <remote>/<branch> ref, not a hardcoded origin/main.
func TestFormatGate_NonMainBaseUsesResolvedRef(t *testing.T) {
	fg := newFakeGit().
		on("diff --name-only --diff-filter=ACMR origin/eks-pipeline...HEAD", scriptedResponse{
			stdout: "main.go\n", exitCode: 0,
		})
	fc := newFakeFormatCmd().
		on("gofmt -l main.go", scriptedResponse{stdout: "", exitCode: 0})

	err := FormatGate(FormatGateOpts{
		Worktree:  "/fake/worktree",
		Branch:    "eks-pipeline",
		gitRunner: fg,
		cmdRunner: fc,
		lookPath:  fakeLookPath("gofmt"),
	})
	if err != nil {
		t.Fatalf("FormatGate: unexpected error: %v", err)
	}
	if !fg.called("diff --name-only --diff-filter=ACMR origin/eks-pipeline...HEAD") {
		t.Errorf("FormatGate: expected diff against resolved base ref; calls=%v", fg.calls)
	}
}

// TestFormatGate_GoSyntaxErrorRefusesWithDiagnostic verifies that a gofmt
// invocation exiting non-zero for a reason other than "needs formatting"
// (e.g. a parse error) still refuses, rather than being treated as a clean
// pass.
func TestFormatGate_GoSyntaxErrorRefusesWithDiagnostic(t *testing.T) {
	fg := newFakeGit().
		on("diff --name-only --diff-filter=ACMR origin/main...HEAD", scriptedResponse{
			stdout: "broken.go\n", exitCode: 0,
		})
	fc := newFakeFormatCmd().
		on("gofmt -l broken.go", scriptedResponse{
			stderr:   "broken.go:2:6: expected 'IDENT', found '{'",
			exitCode: 2,
		})

	err := FormatGate(FormatGateOpts{
		Worktree:  "/fake/worktree",
		gitRunner: fg,
		cmdRunner: fc,
		lookPath:  fakeLookPath("gofmt"),
	})
	if err == nil {
		t.Fatal("FormatGate: expected refusal on gofmt parse error, got nil")
	}
	var fe *FormatGateError
	if !errors.As(err, &fe) {
		t.Fatalf("FormatGate: error is not *FormatGateError: %T %v", err, err)
	}
	if !strings.Contains(fe.Msg, "broken.go") {
		t.Errorf("FormatGate: refusal missing offending file; got:\n%s", fe.Msg)
	}
}
