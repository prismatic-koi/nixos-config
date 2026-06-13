// Tests for the `prism account login` cobra wiring. The bulk of
// behavioural coverage lives in internal/account/login_test.go; these
// only exercise the cobra-level surface (flag parsing, name arg
// validation that short-circuits before any network call).
package cmd

import (
	"strings"
	"testing"
)

func TestAccountLogin_RegisteredOnAccountCmd(t *testing.T) {
	var found bool
	for _, c := range accountCmd.Commands() {
		if c.Name() == "login" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("login subcommand not registered on accountCmd")
	}
}

func TestAccountLogin_HasUseAndPortFlags(t *testing.T) {
	if accountLoginCmd.Flag("use") == nil {
		t.Error("--use flag not defined")
	}
	if accountLoginCmd.Flag("port") == nil {
		t.Error("--port flag not defined")
	}
	// Default port should match the auth.ts CALLBACK_PORT constant.
	if got := accountLoginCmd.Flag("port").DefValue; got != "53692" {
		t.Errorf("--port default = %q, want 53692", got)
	}
}

func TestAccountLogin_InvalidName_FailsBeforeNetworkCall(t *testing.T) {
	withAccountFixture(t)
	out, err := runSubcommand(t, runAccountLogin, []string{".."})
	if err == nil {
		t.Fatal("expected error for invalid name")
	}
	// Should not have printed an authorize URL (proxy for "no network
	// call attempted").
	if strings.Contains(out, "claude.ai/oauth/authorize") {
		t.Errorf("invalid-name path printed authorize URL: %q", out)
	}
}
