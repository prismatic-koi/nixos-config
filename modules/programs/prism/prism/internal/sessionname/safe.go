package sessionname

import (
	"fmt"
	"strings"
)

// The safe sets keep a name out of shell syntax. tmux.nix puts a session
// name into shell commands through tmux formats (status-right, the
// session-closed and pane-exited hooks, and key bindings), and each place
// quotes it with #{q:...}. The safe set is the second layer: a name in the
// set reads the same to a shell with or without the quote.
//
// #{q:...} does not escape "~", and a shell expands "~" at the start of a
// word. So a session name must start with a letter or a digit.
const (
	// SessionNameSafeSet describes a valid session name.
	SessionNameSafeSet = `ASCII letters, digits, "-", "_", "@", and "~", with a letter or a digit first`

	// BranchSafeSet describes a branch name that prism accepts for a
	// session. session.NameFor maps "/" to "--" and "." to "_", so each
	// branch in this set gives a session name in SessionNameSafeSet.
	BranchSafeSet = `ASCII letters, digits, "-", "_", ".", and "/"`

	sessionNameExtra = "-_@~"
	branchExtra      = "-_./"
)

// ValidateName returns an error when name is not in SessionNameSafeSet. The
// error names each character outside the set.
func ValidateName(name string) error {
	if err := validate("session name", name, sessionNameExtra, SessionNameSafeSet); err != nil {
		return err
	}
	if !isASCIIAlnum(rune(name[0])) {
		return fmt.Errorf("session name %q starts with %q, and the safe set needs a letter or a digit first (the safe set is %s)",
			name, name[:1], SessionNameSafeSet)
	}
	return nil
}

// ValidateBranch returns an error when branch is empty or holds a character
// outside BranchSafeSet. The error names each such character. git applies
// its own rules for a branch name after this check.
func ValidateBranch(branch string) error {
	return validate("branch name", branch, branchExtra, BranchSafeSet)
}

func validate(kind, value, extra, set string) error {
	if value == "" {
		return fmt.Errorf("%s is empty", kind)
	}
	var bad []string
	for _, r := range value {
		if isASCIIAlnum(r) || strings.ContainsRune(extra, r) {
			continue
		}
		q := fmt.Sprintf("%q", string(r))
		if !containsString(bad, q) {
			bad = append(bad, q)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	noun := "the character"
	if len(bad) > 1 {
		noun = "the characters"
	}
	return fmt.Errorf("%s %q holds %s %s, which the safe set does not allow (the safe set is %s)",
		kind, value, noun, strings.Join(bad, ", "), set)
}

func isASCIIAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
