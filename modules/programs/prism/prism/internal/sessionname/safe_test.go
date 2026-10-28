package sessionname

import (
	"strings"
	"testing"
)

// TestValidateName_AcceptsNamesInUse pins the session names that prism makes
// today. Each shape is taken from the sessions table: every name there holds
// only letters, digits, "-", "_", "@", and "~".
func TestValidateName_AcceptsNamesInUse(t *testing.T) {
	for _, name := range []string{
		"nixos-config@main",
		"nixos-config@tmux-name-injection",
		"nixos-config@pi-shared-state-dirs~review-2-review-code",
		"nixos-config@feat~review-1-review-goal-fallback-1",
		"nixos-config@main~investigate-flake",
		"home-ops@20260828T2056",
		"home-ops@prismatic-bot--kube-prometheus-stack-89_x",
		"typ-mariadb-iam-auth@main",
		"obsidian",
		"obsidian~investigate-v2",
		"weird~repo@main",
		"scratchpad",
		"prism-dashboard",
		"prism-test@x",
		"0repo@main",
	} {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", name, err)
		}
	}
}

// TestValidateName_RefusesShellSyntax pins the refusal and the error text.
// The error must name each character that is outside the safe set.
func TestValidateName_RefusesShellSyntax(t *testing.T) {
	tests := []struct {
		name     string
		wantBad  []string
		wantNone []string
	}{
		{"x$(touch pwned)", []string{`"$"`, `"("`, `")"`, `" "`}, nil},
		{"repo@x`id`", []string{"\"`\""}, nil},
		{"repo@a;b", []string{`";"`}, nil},
		{"repo@a|b", []string{`"|"`}, nil},
		{"repo@a&b", []string{`"&"`}, nil},
		{"repo@a'b", []string{`"'"`}, nil},
		{`repo@a"b`, []string{`"\""`}, nil},
		{`repo@a\b`, []string{`"\\"`}, nil},
		{"repo@a b", []string{`" "`}, nil},
		{"repo@a\tb", []string{`"\t"`}, nil},
		{"repo@a\nb", []string{`"\n"`}, nil},
		{"repo@a#b", []string{`"#"`}, nil},
		{"repo@a*b", []string{`"*"`}, nil},
		{"repo@a.b", []string{`"."`}, nil},
		{"repo@a/b", []string{`"/"`}, nil},
		{"repo@a:b", []string{`":"`}, nil},
		{"repo@é", []string{`"é"`}, nil},
		// A repeated character is named once.
		{"repo@$$$", []string{`"$"`}, []string{`"$", "$"`}},
	}
	for _, tt := range tests {
		err := ValidateName(tt.name)
		if err == nil {
			t.Errorf("ValidateName(%q) = nil, want an error", tt.name)
			continue
		}
		msg := err.Error()
		for _, want := range tt.wantBad {
			if !strings.Contains(msg, want) {
				t.Errorf("ValidateName(%q) error %q does not name %s", tt.name, msg, want)
			}
		}
		for _, notWant := range tt.wantNone {
			if strings.Contains(msg, notWant) {
				t.Errorf("ValidateName(%q) error %q holds %s", tt.name, msg, notWant)
			}
		}
		if !strings.Contains(msg, SessionNameSafeSet) {
			t.Errorf("ValidateName(%q) error %q does not state the safe set", tt.name, msg)
		}
	}
}

// TestValidateName_FirstCharacter pins the rule that a session name starts
// with a letter or a digit. #{q:...} does not escape "~", so a name that
// starts with "~" reads to a shell as a home directory.
func TestValidateName_FirstCharacter(t *testing.T) {
	for _, name := range []string{"~root", "-x@main", "_x@main", "@main"} {
		err := ValidateName(name)
		if err == nil {
			t.Errorf("ValidateName(%q) = nil, want an error", name)
			continue
		}
		if !strings.Contains(err.Error(), "letter or a digit first") {
			t.Errorf("ValidateName(%q) error %q does not state the first-character rule", name, err)
		}
	}
}

func TestValidateName_Empty(t *testing.T) {
	if err := ValidateName(""); err == nil {
		t.Error(`ValidateName("") = nil, want an error`)
	}
}

// TestValidateBranch_AcceptsBranchesInUse pins the branch shapes that `git
// branch -a` shows in the prism repos today: letters, digits, "-", "_", "/",
// and ".".
func TestValidateBranch_AcceptsBranchesInUse(t *testing.T) {
	for _, branch := range []string{
		"main",
		"tmux-name-injection",
		"20260531T1213",
		"bump-pi-0.77.0",
		"pi-0.79.1",
		"prismatic-bot/kube-prometheus-stack-89.x",
		"renovate/github-actions",
		"snake_case_branch",
		"UPPER",
	} {
		if err := ValidateBranch(branch); err != nil {
			t.Errorf("ValidateBranch(%q) = %v, want nil", branch, err)
		}
	}
}

func TestValidateBranch_RefusesShellSyntax(t *testing.T) {
	tests := []struct {
		branch  string
		wantBad []string
	}{
		{"x$(touch pwned)", []string{`"$"`, `"("`, `")"`, `" "`}},
		{"x`id`", []string{"\"`\""}},
		{"a;b", []string{`";"`}},
		{"a~b", []string{`"~"`}},
		{"a@b", []string{`"@"`}},
		{"a:b", []string{`":"`}},
		{"a b", []string{`" "`}},
		{"a'b", []string{`"'"`}},
	}
	for _, tt := range tests {
		err := ValidateBranch(tt.branch)
		if err == nil {
			t.Errorf("ValidateBranch(%q) = nil, want an error", tt.branch)
			continue
		}
		for _, want := range tt.wantBad {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("ValidateBranch(%q) error %q does not name %s", tt.branch, err, want)
			}
		}
		if !strings.Contains(err.Error(), BranchSafeSet) {
			t.Errorf("ValidateBranch(%q) error %q does not state the safe set", tt.branch, err)
		}
	}
	if err := ValidateBranch(""); err == nil {
		t.Error(`ValidateBranch("") = nil, want an error`)
	}
}
