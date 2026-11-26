package ghout

import (
	"os/exec"
	"strings"
	"testing"
)

func TestRun_SuccessDropsStderr(t *testing.T) {
	out, err := Run(exec.Command("sh", "-c", `echo '{"a":1}'; echo 'hint: x' >&2`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != `{"a":1}` {
		t.Fatalf("got %q", out)
	}
}

func TestRun_FailureKeepsStderr(t *testing.T) {
	out, err := Run(exec.Command("sh", "-c", `echo 'boom' >&2; exit 3`))
	if err == nil || !strings.Contains(string(out), "boom") {
		t.Fatalf("got %q, %v", out, err)
	}
}
