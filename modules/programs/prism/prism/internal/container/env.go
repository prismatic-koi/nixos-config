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
// slice. It handles three sources:
//
//  1. cfg.AgentEnvVars — profile-level vars (e.g. GIT_EDITOR=true) emitted
//     in sorted key order for determinism. Every key flows through — there
//     is no suppression map. kubectl resolves the kube config via KUBECONFIG
//     at the host XDG path (bwrap binds that path Dst==Src — see
//     StandardSandboxMounts; sandbox-exec reads ride the secrets.d allowlist).
//
//  2. cfg.RuntimeEnv — harness-specific runtime vars (e.g. the bash-tool
//     timeout) emitted as-is.
//
//  3. cfg.AgentCredentials — the variables of each agent credential of the
//     session (agentCredentialEnv). Emitted last, so a profile-level var of
//     the same name cannot replace them.
//
// The caller is responsible for any additional env vars that are
// mode-specific (e.g. NIX_CONFIG, TERM, COLORTERM, KUBECACHEDIR, PRISM_*
// context vars).
func AppendStandardEnv(args []string, cfg Config, appender EnvAppender) []string {
	if len(cfg.AgentEnvVars) > 0 {
		keys := make([]string, 0, len(cfg.AgentEnvVars))
		for k := range cfg.AgentEnvVars {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = appender(args, k, cfg.AgentEnvVars[k])
		}
	}

	for k, v := range cfg.RuntimeEnv {
		args = appender(args, k, v)
	}

	for _, kv := range agentCredentialEnv(cfg) {
		args = appender(args, kv[0], kv[1])
	}

	return args
}

// agentCredentialEnv returns the agent credential variables of cfg in sorted
// order, or nil when the session gets no credential.
func agentCredentialEnv(cfg Config) [][2]string {
	vars := cfg.AgentCredentials.EnvVars()
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([][2]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, [2]string{k, vars[k]})
	}
	return out
}

// AppendSandboxEnvVarsKV appends K=V pairs from cfg.AgentEnvVars,
// cfg.RuntimeEnv, and cfg.AgentCredentials to env (a []string slice for use as a
// syscall.Exec env).
// Like AppendStandardEnv, every AgentEnvVars key flows through — there is no
// suppression map. The kube config is read via KUBECONFIG at the host XDG path.
//
// This is used by the sandbox-exec dispatch path in cmd/agent_run.go where
// the env is a plain K=V slice (not a bwrap-style argument list).
func AppendSandboxEnvVarsKV(env []string, cfg Config) []string {
	if len(cfg.AgentEnvVars) > 0 {
		keys := make([]string, 0, len(cfg.AgentEnvVars))
		for k := range cfg.AgentEnvVars {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			env = append(env, k+"="+cfg.AgentEnvVars[k])
		}
	}

	for k, v := range cfg.RuntimeEnv {
		env = append(env, k+"="+v)
	}

	for _, kv := range agentCredentialEnv(cfg) {
		env = append(env, kv[0]+"="+kv[1])
	}

	return env
}
