package session

import (
	"errors"
	"testing"
)

func TestHasExactSession_ExactNamesOnly(t *testing.T) {
	old := listTmuxSessions
	t.Cleanup(func() { listTmuxSessions = old })
	listTmuxSessions = func() (string, error) {
		return "repo@feature-x\nrepo@feat~review-1-review-goal\nrepo@a.b:c\n", nil
	}
	if HasExactSession("repo@feat") {
		t.Error("prefix of a live name matched")
	}
	if !HasExactSession("repo@a.b:c") {
		t.Error("name with . and : not matched whole")
	}
	listTmuxSessions = func() (string, error) { return "", errors.New("boom") }
	if HasExactSession("repo@feat") {
		t.Error("unknown tmux state reported as present")
	}
}
