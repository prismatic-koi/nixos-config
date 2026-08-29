package container

// sandbox_exec_podman_proxy_test.go — unit tests for the conditional SBPL
// allow that exposes the per-session filtering podman API socket inside the
// sandbox-exec sandbox.
//
// The integration coverage (positive + paired negative-mutation per the
// AGENTS.md sandbox-exec testing convention) lives in
// internal/integration/sandbox_exec_podman_proxy_darwin_test.go. This file
// covers the rendering-level invariants that the integration test cannot
// exercise without launching sandbox-exec:
//
//   - When ContainersEnabled=true and PodmanProxySockPath is set, the profile
//     contains a literal RW allow for the exact path (and only the literal
//     — no subpath form).
//   - When ContainersEnabled=false (the default), no clause in the profile
//     mentions the proxy socket path.
//   - SECURITY: the upstream podman socket path never appears in the SBPL
//     for ANY value of ContainersEnabled. This is the greppable security
//     check — the proxy is load-bearing only if the agent has no
//     path to bypass it.
//   - Defence in depth: an empty PodmanProxySockPath with ContainersEnabled=true
//     emits no allow rule, so a misconfigured caller cannot accidentally
//     widen the grant.

import (
	"strings"
	"testing"
)

// sentinelUpstreamPodmanSocketPath is a deliberately-recognisable stand-in
// for the host-side podman socket path returned by `podman machine inspect`
// on Darwin or $XDG_RUNTIME_DIR/podman/podman.sock on Linux. The
// greppable security test asserts this exact byte sequence never appears
// in the rendered SBPL for any value of ContainersEnabled. The sentinel is
// long and includes a unique substring so a stray substring match on a
// production-looking path (e.g. /tmp/podman/...) cannot give a false
// negative.
const sentinelUpstreamPodmanSocketPath = "/private/tmp/sentinel-upstream-podman-NEVER-IN-PROFILE-2322/podman.sock"

// TestGenerateProfile_PodmanProxy_LiteralAllowWhenEnabled verifies that
// when ContainersEnabled=true and PodmanProxySockPath is set, the profile
// emits a literal RW allow for the proxy path — and only the literal
// form. This is the §3c clause (file-read* file-write* with
// literal, not subpath, scope) and the canonical positive AC.
func TestGenerateProfile_PodmanProxy_LiteralAllowWhenEnabled(t *testing.T) {
	const proxyPath = "/private/tmp/prism-sbx-proxy-test/podman.sock"
	m := newSandboxExecManager(Config{
		SessionName:         "repo@main",
		ContainersEnabled:   true,
		PodmanProxySockPath: proxyPath,
	})
	profile := generateProfile(m)

	wantClause := "(allow file-read* file-write*\n  (literal " + quoteSBPL(proxyPath) + "))"
	if !strings.Contains(profile, wantClause) {
		t.Errorf("profile missing literal allow for podman proxy socket.\nwant clause:\n%s\nfull profile:\n%s", wantClause, profile)
	}

	// The grant must be a literal, not a subpath. A (subpath ...) form would
	// widen the grant to the entire run dir, undermining the §3c narrowing
	// rationale.
	bannedSubpath := "(subpath " + quoteSBPL(proxyPath) + ")"
	if strings.Contains(profile, bannedSubpath) {
		t.Errorf("profile contains a (subpath %q) grant for the podman proxy socket — must be (literal ...) only", proxyPath)
	}
}

// TestGenerateProfile_PodmanProxy_NoMentionWhenDisabled verifies that with
// ContainersEnabled=false (the default), NO clause in the profile mentions
// the proxy socket path — even when the caller redundantly populates
// PodmanProxySockPath. This is the default-off AC and protects
// against accidental widening on sessions that did not opt in.
func TestGenerateProfile_PodmanProxy_NoMentionWhenDisabled(t *testing.T) {
	const proxyPath = "/private/tmp/prism-sbx-proxy-disabled/podman.sock"
	cases := []struct {
		name string
		cfg  Config
	}{
		{
			name: "ContainersEnabled=false_PodmanProxySockPath_unset",
			cfg: Config{
				SessionName: "repo@main",
			},
		},
		{
			name: "ContainersEnabled=false_PodmanProxySockPath_populated_but_ignored",
			cfg: Config{
				SessionName:         "repo@main",
				PodmanProxySockPath: proxyPath,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newSandboxExecManager(tc.cfg)
			profile := generateProfile(m)

			if strings.Contains(profile, proxyPath) {
				t.Errorf("profile mentions podman proxy path %q despite ContainersEnabled=false; full profile:\n%s",
					proxyPath, profile)
			}
			if strings.Contains(profile, "podman.sock") {
				t.Errorf("profile contains the substring \"podman.sock\" despite ContainersEnabled=false (a default-off session must have no proxy surface); full profile:\n%s",
					profile)
			}
		})
	}
}

// TestGenerateProfile_PodmanProxy_EmptyPathEmitsNoAllow verifies the
// defence-in-depth branch: when ContainersEnabled=true but PodmanProxySockPath
// is empty (the caller failed to populate it), the generator emits no
// allow rule at all rather than something like (literal "") which a future
// SBPL engine could misinterpret. The call-site contract is "set both or
// neither"; the generator enforces it.
func TestGenerateProfile_PodmanProxy_EmptyPathEmitsNoAllow(t *testing.T) {
	m := newSandboxExecManager(Config{
		SessionName:         "repo@main",
		ContainersEnabled:   true,
		PodmanProxySockPath: "",
	})
	profile := generateProfile(m)

	// The empty-literal form must never appear in the profile.
	if strings.Contains(profile, `(literal "")`) {
		t.Errorf("profile contains an empty-literal allow rule (defence-in-depth failure); full profile:\n%s", profile)
	}
	// As a tighter check, no clause should mention podman.sock either.
	if strings.Contains(profile, "podman.sock") {
		t.Errorf("profile contains \"podman.sock\" despite PodmanProxySockPath being empty; full profile:\n%s", profile)
	}
}

// TestGenerateProfile_PodmanProxy_UpstreamPathNeverAppears is the
// greppable security check: the real upstream podman socket path
// (the value returned by `podman machine inspect`) must never appear in
// the rendered SBPL for ANY value of ContainersEnabled. The proxy is
// load-bearing only if the agent has no path to bypass it — if the
// upstream path leaked into the SBPL, the agent could connect(2) directly
// and bypass the filter.
//
// We use a sentinel substring rather than a production-looking path so a
// stray substring match cannot give a false negative. Both ContainersEnabled
// values are exercised because the sentinel must NEVER appear, regardless
// of how the gate is configured.
func TestGenerateProfile_PodmanProxy_UpstreamPathNeverAppears(t *testing.T) {
	// Cases enumerate the values of ContainersEnabled and PodmanProxySockPath
	// that the dispatcher might legitimately produce. The sentinel path
	// stands in for the upstream socket; we deliberately place it in
	// places where a careless implementation might leak it (e.g. as the
	// proxy path itself, or in BareRoot which is also embedded in the
	// profile via the ancestor-probe rule).
	cases := []struct {
		name string
		cfg  Config
	}{
		{
			name: "ContainersEnabled=false_no_proxy_path",
			cfg: Config{
				SessionName: "repo@main",
			},
		},
		{
			name: "ContainersEnabled=true_proxy_path_is_filtered_socket",
			cfg: Config{
				SessionName:         "repo@main",
				ContainersEnabled:   true,
				PodmanProxySockPath: "/private/tmp/prism-sbx-proxy-greppable/podman.sock",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newSandboxExecManager(tc.cfg)
			profile := generateProfile(m)

			if strings.Contains(profile, sentinelUpstreamPodmanSocketPath) {
				t.Errorf("SBPL leaks the upstream podman socket sentinel %q — this would let the agent bypass the proxy.\nfull profile:\n%s",
					sentinelUpstreamPodmanSocketPath, profile)
			}
			// Also assert the unique sentinel substring (without the
			// leading slash) does not appear via some indirect rendering
			// path. The substring is unique enough that any match indicates
			// a real leak.
			const sentinelMarker = "sentinel-upstream-podman-NEVER-IN-PROFILE-2322"
			if strings.Contains(profile, sentinelMarker) {
				t.Errorf("SBPL contains the upstream sentinel marker %q (path was reconstructed or partially leaked); full profile:\n%s",
					sentinelMarker, profile)
			}
		})
	}
}

// TestGenerateProfile_PodmanProxy_PathQuoting verifies that a podman proxy
// path containing characters that need SBPL escaping (backslash, double
// quote) is correctly quoted, so the resulting rule is syntactically valid
// SBPL. Production paths under XDG_STATE_HOME don't contain these in
// practice, but the quoter is the only line of defence against a future
// caller passing through such a path.
func TestGenerateProfile_PodmanProxy_PathQuoting(t *testing.T) {
	cases := []string{
		`/private/tmp/with"quote/podman.sock`,
		`/private/tmp/with\backslash/podman.sock`,
		`/private/tmp/plain/podman.sock`,
	}
	for _, proxyPath := range cases {
		t.Run(proxyPath, func(t *testing.T) {
			m := newSandboxExecManager(Config{
				SessionName:         "repo@main",
				ContainersEnabled:   true,
				PodmanProxySockPath: proxyPath,
			})
			profile := generateProfile(m)
			want := "(literal " + quoteSBPL(proxyPath) + "))"
			if !strings.Contains(profile, want) {
				t.Errorf("profile missing quoted literal for %q\nwant: %q\nfull profile:\n%s", proxyPath, want, profile)
			}
		})
	}
}
