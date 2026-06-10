package tmuxtest

import "testing"

// TestIsSandboxForkDenial verifies the fork-denial classifier matches the
// exact in-sandbox signature (issue #2204) and nothing broader — the guard
// must not skip on unrelated tmux failures (no over-skip).
func TestIsSandboxForkDenial(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		output string
		want   bool
	}{
		{
			name:   "observed in-sandbox signature",
			output: "create window failed: fork failed: Operation not permitted\n",
			want:   true,
		},
		{
			name:   "bare fork denial",
			output: "fork failed: Operation not permitted",
			want:   true,
		},
		{
			name:   "empty output",
			output: "",
			want:   false,
		},
		{
			name:   "unrelated tmux error",
			output: "error connecting to /tmp/tmux-501/default (No such file or directory)",
			want:   false,
		},
		{
			name:   "permission error without fork",
			output: "open: Operation not permitted",
			want:   false,
		},
		{
			name:   "fork failure with different errno",
			output: "create window failed: fork failed: Resource temporarily unavailable",
			want:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsSandboxForkDenial(tc.output); got != tc.want {
				t.Errorf("IsSandboxForkDenial(%q) = %v, want %v", tc.output, got, tc.want)
			}
		})
	}
}
