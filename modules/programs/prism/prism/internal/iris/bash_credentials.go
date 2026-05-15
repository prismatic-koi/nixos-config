package iris

// bash_credentials.go — bash-specific credential helper for D-5.
//
// The bash subprocess receives a role-scoped GITHUB_TOKEN selected from the
// 4-PAT architecture (account × role → token), reusing the logic already
// implemented in internal/container/credentials.go via the exported
// container.GithubTokenForBareRootAndRole free function.
//
// LLM API keys (ANTHROPIC_API_KEY, OPENROUTER_API_KEY) are explicitly excluded:
// bash subprocesses must not be able to make LLM calls.
//
// AWS environment variables are NOT injected here — the bash subprocess
// reads the AWS credentials from the mounted ~/.aws/{config,credentials}
// files directly (the AWS CLI resolves them from the file-based credential
// chain). AWS_CONFIG_FILE and AWS_SHARED_CREDENTIALS_FILE are set to point
// at the in-sandbox canonical paths.

import (
	"os"

	"github.com/prismatic-koi/prism/internal/container"
)

// bashEnv returns the complete environment variable list for a bash subprocess.
// It includes:
//   - PATH, HOME, USER, LOGNAME, LANG, LC_ALL (standard shell env)
//   - TERM (terminal type for interactive tools)
//   - GIT_AUTHOR_NAME, GIT_AUTHOR_EMAIL, GIT_COMMITTER_NAME, GIT_COMMITTER_EMAIL
//     (git identity, forwarded from host so bash commands can commit)
//   - NIX_CONFIG (point at the nix daemon)
//   - GITHUB_TOKEN (role-scoped, from 4-PAT architecture via container package)
//   - AWS_CONFIG_FILE, AWS_SHARED_CREDENTIALS_FILE (point at in-sandbox paths)
//
// Explicitly NOT included:
//   - ANTHROPIC_API_KEY (LLM API key — bash must not make LLM calls)
//   - OPENROUTER_API_KEY (LLM API key — bash must not make LLM calls)
//   - Any pi-process-specific credentials (~/.claude, ~/.mcp-auth, etc.)
func bashEnv(role, bareRoot string) []string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}

	var env []string

	// Standard shell environment.
	pathVal := os.Getenv("PATH")
	if pathVal == "" {
		pathVal = "/run/current-system/sw/bin:/nix/var/nix/profiles/default/bin:/usr/bin:/bin"
	}
	env = append(env, "PATH="+pathVal)

	for _, key := range []string{"HOME", "USER", "LOGNAME", "LANG", "LC_ALL"} {
		if val := os.Getenv(key); val != "" {
			env = append(env, key+"="+val)
		}
	}

	// Terminal type.
	if term := os.Getenv("TERM"); term != "" {
		env = append(env, "TERM="+term)
	}

	// Git identity — forwarded so bash can run git commit.
	for _, key := range []string{
		"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL",
		"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL",
	} {
		if val := os.Getenv(key); val != "" {
			env = append(env, key+"="+val)
		}
	}

	// Nix daemon.
	env = append(env, "NIX_CONFIG=store = daemon")

	// Role-scoped GitHub token — reuses the 4-PAT architecture from the
	// container package.  LLM API keys are deliberately excluded here.
	if tok := container.GithubTokenForBareRootAndRole(bareRoot, role); tok != "" {
		env = append(env, "GITHUB_TOKEN="+tok)
	}

	// AWS credential file paths — point at the in-sandbox canonical paths.
	// These match the MountSpec SandboxPaths for ~/.aws/config and
	// ~/.aws/credentials.  Bwrap uses Dst==Src mounts so the in-sandbox
	// paths equal the host paths.
	env = append(env, "AWS_CONFIG_FILE="+home+"/.aws/config")
	env = append(env, "AWS_SHARED_CREDENTIALS_FILE="+home+"/.aws/credentials")

	return env
}
