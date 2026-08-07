package session

import "testing"

func TestDeriveFallbackTitle(t *testing.T) {
	cases := []struct {
		name   string
		prompt string
		want   string
	}{
		{"empty prompt", "", ""},
		{"whitespace only", "   \n\t\n  ", ""},
		{"single line", "Fix the login bug", "Fix the login bug"},
		{
			"first non-blank line, later lines ignored",
			"\n\n  Fix GitHub issue #2641: spawned worker sessions have no title\n\nMore detail below.\n",
			"Fix GitHub issue #2641: spawned worker sessions have no title",
		},
		{
			"internal whitespace collapsed",
			"Fix   the    login\tbug   ",
			"Fix the login bug",
		},
		{
			"truncates long first line with ellipsis",
			"This is a very long prompt title that goes on and on and on and on and on and on and on and on forever",
			"This is a very long prompt title that goes on and on and on and on and on and o…",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveFallbackTitle(tc.prompt)
			if got != tc.want {
				t.Errorf("deriveFallbackTitle(%q) = %q, want %q", tc.prompt, got, tc.want)
			}
			if len([]rune(got)) > fallbackTitleMaxRunes {
				t.Errorf("deriveFallbackTitle(%q) = %q, exceeds max runes %d", tc.prompt, got, fallbackTitleMaxRunes)
			}
		})
	}
}
