package tmux

import "testing"

// TestMostRecentClient exercises the pure selection logic behind
// ClientForSession without needing a live tmux server. It covers the empty
// case (no client attached -> ""), the single-client case, the multi-client
// tiebreak (most recent activity wins), and malformed input.
func TestMostRecentClient(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty input", "", ""},
		{"single client", "1785618025|/dev/pts/3", "/dev/pts/3"},
		{"picks most recent activity", "100|/dev/pts/1\n200|/dev/pts/2\n150|/dev/pts/3", "/dev/pts/2"},
		{"blank lines ignored", "\n  \n300|/dev/pts/9\n", "/dev/pts/9"},
		{"malformed line skipped", "garbage-no-delimiter\n50|/dev/pts/4", "/dev/pts/4"},
		{"empty client name skipped", "999|\n10|/dev/pts/5", "/dev/pts/5"},
	}
	for _, c := range cases {
		if got := mostRecentClient(c.in); got != c.want {
			t.Errorf("%s: mostRecentClient(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}
