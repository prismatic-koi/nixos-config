//go:build !darwin

package session

// platformSandboxFinder returns no finder, so the stop does nothing. A
// bwrap session runs in a PID namespace of its own (--unshare-pid), and
// the kernel ends each process of the namespace when the agent ends. A
// host session has no sandbox.
func platformSandboxFinder(string) (sandboxFinder, error) { return nil, nil }
