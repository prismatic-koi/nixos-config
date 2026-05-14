package investigate_test

import (
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/investigate"
)

func TestValidateName(t *testing.T) {
	valid := []string{
		"foo",
		"foo-bar",
		"my-analysis",
		"abc123",
		"a",
		"z",
		strings.Repeat("a", 40), // exactly 40 chars
	}
	for _, name := range valid {
		if err := investigate.ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) unexpected error: %v", name, err)
		}
	}

	invalid := []struct {
		name        string
		mustContain string
	}{
		{strings.Repeat("a", 41), "maximum is 40"},
		{"-leading", "start or end with a dash"},
		{"trailing-", "start or end with a dash"},
		{"UPPER", "disallowed characters"},
		{"has spaces", "disallowed characters"},
		{"Has Spaces", "disallowed characters"},
		{"under_score", "disallowed characters"},
		{"foo@bar", "disallowed characters"},
	}
	for _, tc := range invalid {
		err := investigate.ValidateName(tc.name)
		if err == nil {
			t.Errorf("ValidateName(%q) expected error, got nil", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.mustContain) {
			t.Errorf("ValidateName(%q): error %q does not contain %q", tc.name, err.Error(), tc.mustContain)
		}
	}
}
