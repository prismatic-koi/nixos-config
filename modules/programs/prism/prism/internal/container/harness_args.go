package container

import "fmt"

// HarnessInvocation returns the trailing arg slice that launches opencode
// with the per-session port, role, and initial prompt. The leading args
// (sandbox-exec wrapper, bwrap "--" terminator, etc.) are the isolator's
// responsibility.
//
// Port selection follows the AllocatedPort ∥ ContainerPort fallback rule:
// non-podman isolators share the host network namespace and bind directly
// to a per-session host port (cfg.AllocatedPort). ContainerPort is retained
// as a fallback for the theoretical case where AllocatedPort is unset.
//
// The hostname is always 127.0.0.1 for non-podman modes (host network
// namespace is shared; binding 0.0.0.0 would be overly broad). Podman keeps
// its own inline invocation tail with 0.0.0.0 and ContainerPort.
func HarnessInvocation(cfg Config) []string {
	port := cfg.AllocatedPort
	if port == 0 {
		port = ContainerPort
	}
	args := []string{
		"opencode",
		"--port", fmt.Sprintf("%d", port),
		"--hostname", "127.0.0.1",
	}
	if cfg.AgentRole != "" {
		args = append(args, "--agent", cfg.AgentRole)
	}
	if cfg.InitialPrompt != "" {
		args = append(args, "--prompt", cfg.InitialPrompt)
	}
	return args
}
