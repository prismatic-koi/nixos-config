// Package ghout runs gh and keeps its stdout apart from its stderr.
//
// gh 2.105.0 prints a "hint:" block on stderr when it detects an agent
// caller. A caller that parses the JSON from combined output fails on it.
package ghout

import (
	"bytes"
	"os/exec"
)

// Run runs cmd. On success it returns stdout only. On failure it returns
// stdout followed by stderr, so the error text of gh stays visible and
// callers can match on it.
func Run(cmd *exec.Cmd) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return append(stdout.Bytes(), stderr.Bytes()...), err
	}
	return stdout.Bytes(), nil
}
