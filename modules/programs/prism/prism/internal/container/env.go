package container

import (
	"sort"
)

// EnvAppender appends a single environment variable (key, value) to args in
// the isolator's native syntax and returns the extended slice. Bwrap uses
// "--setenv", "K", "V".
type EnvAppender func(args []string, k, v string) []string

// AppendStandardEnv appends the per-session environment variables derived
// from cfg to args using the provided appender, and returns the extended
// slice. It handles two sources:
//
//  1. cfg.AgentEnvVars — profile-level vars (e.g. GIT_EDITOR=true) emitted
//     in sorted key order for determinism. KUBECONFIG is suppressed because
//     the kube config is still bind-mounted at its canonical default path
//     inside the sandbox and the host XDG path held in AgentEnvVars would
//     only override the correctly-mounted location (un-suppression is
//     Step 3b of #2132). AWS_CONFIG_FILE and AWS_SHARED_CREDENTIALS_FILE
//     are NOT suppressed (issue #2234, Step 3a of #2132): the canonical-path
//     ($HOME/.aws/*) delivery for the aws config/credentials was dropped
//     from both isolators, and the aws CLI resolves them via these env vars
//     at the host XDG paths instead (bwrap binds those XDG paths Dst==Src —
//     see StandardSandboxMounts; sandbox-exec reads ride the #2211
//     allowlist).
//
//  2. cfg.RuntimeEnv — harness-specific runtime vars (e.g. the bash-tool
//     timeout) emitted as-is.
//
// The caller is responsible for any additional env vars that are
// mode-specific (e.g. NIX_CONFIG, TERM, COLORTERM, PRISM_* context vars).
func AppendStandardEnv(args []string, cfg Config, appender EnvAppender) []string {
	// Suppress keys that are already provided via bind-mounts.
	sandboxMountedByDefault := map[string]bool{
		"KUBECONFIG": true,
	}

	if len(cfg.AgentEnvVars) > 0 {
		keys := make([]string, 0, len(cfg.AgentEnvVars))
		for k := range cfg.AgentEnvVars {
			if !sandboxMountedByDefault[k] {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = appender(args, k, cfg.AgentEnvVars[k])
		}
	}

	for k, v := range cfg.RuntimeEnv {
		args = appender(args, k, v)
	}

	return args
}

// AppendSandboxEnvVarsKV appends K=V pairs from cfg.AgentEnvVars and
// cfg.RuntimeEnv to env (a []string slice for use as a syscall.Exec env).
// It applies the same suppression logic as AppendStandardEnv (KUBECONFIG is
// omitted because the kube config is still delivered at its canonical path
// inside the sandbox; the AWS file vars flow through — issue #2234).
//
// This is used by the sandbox-exec dispatch path in cmd/agent_run.go where
// the env is a plain K=V slice (not a bwrap-style argument list).
func AppendSandboxEnvVarsKV(env []string, cfg Config) []string {
	sandboxMountedByDefault := map[string]bool{
		"KUBECONFIG": true,
	}

	if len(cfg.AgentEnvVars) > 0 {
		keys := make([]string, 0, len(cfg.AgentEnvVars))
		for k := range cfg.AgentEnvVars {
			if !sandboxMountedByDefault[k] {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			env = append(env, k+"="+cfg.AgentEnvVars[k])
		}
	}

	for k, v := range cfg.RuntimeEnv {
		env = append(env, k+"="+v)
	}

	return env
}
