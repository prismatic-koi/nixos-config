package doclint

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		in   string
		want tokenClass
	}{
		// --- classSkip: placeholders and non-identifier prose ---
		{"<sessionName>", classSkip},
		{"<path>", classSkip},
		{"path/to/<file>", classSkip},
		{"case \"bind\"", classSkip},
		{"true", classSkip},
		{"allow", classSkip},                 // pure lowercase english word
		{"bind", classSkip},                  // pure lowercase english word
		{"{bind, volume, tmpfs}", classSkip}, // curly-brace set notation
		{"(allow network*)", classSkip},      // SBPL literal (has parens and *)
		{"--containers", classSkip},          // CLI flag
		{"--isolation host", classSkip},      // multi-word
		{"https://example.com", classSkip},   // URL
		{"$HOME", classSkip},                 // $ prefix
		{"~/.config/", classSkip},            // tilde-home
		{"#2317", classSkip},                 // github issue ref
		{"host_bind:*", classSkip},           // wildcard
		{"foo,bar", classSkip},               // comma
		{"a=1", classSkip},                   // assignment
		{"\"json\"", classSkip},              // quoted content

		// --- classFilePath ---
		{"internal/podmanproxy/policy.go", classFilePath},
		{"modules/programs/prism/prism/docs/podman-proxy.md", classFilePath},
		{"docs/rpc.md", classFilePath},

		// --- classFileWithMember ---
		{"policy.go::checkHostConfig", classFileWithMember},
		{"internal/podmanproxy/policy.go::checkHostConfig", classFileWithMember},

		// --- classBareFilename ---
		{"proxy_test.go", classBareFilename},
		{"flake.nix", classBareFilename},
		{"AGENTS.md", classBareFilename},

		// --- classDotted ---
		{"Config.MaxMemoryBytes", classDotted},
		{"agent_status.instance_id", classDotted},

		// --- classGoIdent ---
		{"checkHostConfig", classGoIdent},
		{"NewIsolated", classGoIdent},
		{"hostConfig", classGoIdent},

		// --- classSnakeCase ---
		{"host_bind", classSnakeCase},
		{"agent_max_open_files_soft", classSnakeCase},

		// --- classEnvVar ---
		{"CONTAINER_HOST", classEnvVar},
		{"XDG_STATE_HOME", classEnvVar},
		{"SYS_ADMIN", classEnvVar},

		// --- classColonToken ---
		{"host_bind:<path>", classColonToken},
		{"cap_add:SYS_ADMIN", classColonToken},
	}
	for _, tc := range cases {
		got := classify(tc.in)
		if got != tc.want {
			t.Errorf("classify(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// TestClassify_PlaceholdersNeverFlagged is the load-bearing false-positive
// suppression test called out in the issue AC (edge-case: placeholder tokens
// must not be flagged).
func TestClassify_PlaceholdersNeverFlagged(t *testing.T) {
	placeholders := []string{
		"<sessionName>",
		"<path>",
		"<n>",
		"<t>",
		"<field>_host",
		"<self>",
		"<XDG_STATE_HOME>/prism/run/<sessionDirName>/podman.sock",
		"path/to/<foo>",
	}
	for _, p := range placeholders {
		if got := classify(p); got != classSkip {
			t.Errorf("classify(%q) = %s, want classSkip (placeholder must never be lint-flagged)", p, got)
		}
	}
}
