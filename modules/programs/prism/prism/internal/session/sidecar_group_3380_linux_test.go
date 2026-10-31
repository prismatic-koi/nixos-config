package session

import "testing"

// The command name in /proc/<pid>/stat can hold spaces and parentheses.
// The fields start after the last ')'.
func TestParseProcStat_CommandNameWithParentheses_3380(t *testing.T) {
	state, pgrp, sid, ok := parseProcStat([]byte("4242 (a) (b c) S 1 4100 4000 0 -1 4194304\n"))
	if !ok || state != 'S' || pgrp != 4100 || sid != 4000 {
		t.Errorf("parseProcStat = %c %d %d %v, want S 4100 4000 true", state, pgrp, sid, ok)
	}
	if _, _, _, ok := parseProcStat([]byte("4242 (truncated")); ok {
		t.Error("parseProcStat of a line with no ')' = ok, want not ok")
	}
}
