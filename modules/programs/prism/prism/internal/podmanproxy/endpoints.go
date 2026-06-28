package podmanproxy

import (
	"strings"
)

// endpointKind classifies a request after the path/method allowlist has
// been consulted. The serveHTTP dispatcher uses this to decide whether
// to forward the request as-is, body-inspect first, query-inspect first,
// or reject outright.
type endpointKind int

const (
	// endpointDeny is the default. The request matched no allow rule.
	endpointDeny endpointKind = iota

	// endpointAllow means "forward to the upstream socket without any
	// further inspection". The endpoint is in the allowlist and has no
	// known dangerous fields.
	endpointAllow

	// endpointAllowStreaming means "forward without body parsing". This
	// is identical to endpointAllow in terms of permission, but the
	// dispatcher uses it to short-circuit the read-body-into-buffer
	// path that body-inspecting endpoints take. It exists as a separate
	// constant for clarity: the AC explicitly calls out attach / exec /
	// follow=logs as endpoints that must not parse a body.
	endpointAllowStreaming

	// endpointPolicyCreate means "read body, parse as JSON, apply the
	// containers/create HostConfig policy before forwarding". Used for
	// POST containers/create.
	endpointPolicyCreate

	// endpointPolicyArchive means "inspect the `path` query parameter
	// against the bind-source allowlist before forwarding". Used for
	// PUT containers/{id}/archive (the podman cp write endpoint).
	endpointPolicyArchive

	// endpointPolicyUpdate means "read body, parse as JSON, apply the
	// resource-cap policy in update-context mode (absent fields
	// allowed)". Used for POST containers/{id}/update. Closes the
	// bypass where an agent creates a container with valid caps and
	// then updates them to 0 (= unlimited) after the fact.
	endpointPolicyUpdate

	// endpointPolicyExec means "read body, parse as JSON, reject
	// Privileged=true". Used for POST containers/{id}/exec. Closes the
	// bypass where an agent creates a non-privileged container and
	// then exec's into it with Privileged: true.
	endpointPolicyExec
)

// normalisePath strips a leading docker/podman API version segment and
// an optional libpod/ prefix from p, returning the canonical endpoint
// path WITHOUT a leading slash. Examples:
//
//	"/v1.41/containers/json"        -> "containers/json"
//	"/v5.0.0/libpod/containers/x"   -> "containers/x"
//	"/libpod/containers/create"     -> "containers/create"
//	"/containers/create"            -> "containers/create"
//	"/"                             -> ""
//	"//double"                      -> "" (deny — defence in depth)
//	""                              -> ""
//
// The result is the canonical identifier used by classifyRequest and
// the audit log's policy-rule names. A double-slash or non-leading-slash
// input deliberately returns the empty string so it falls into the
// default-deny branch of classifyRequest rather than producing an
// ambiguous endpoint match.
func normalisePath(p string) string {
	if !strings.HasPrefix(p, "/") {
		return ""
	}
	if strings.HasPrefix(p, "//") {
		return ""
	}
	rest := strings.TrimPrefix(p, "/")
	// Optional version segment, e.g. "v1.41" or "v5.0.0".
	if i := strings.Index(rest, "/"); i > 0 {
		if isVersionSegment(rest[:i]) {
			rest = rest[i+1:]
		}
	}
	// Optional libpod/ namespace prefix (podman-native API).
	rest = strings.TrimPrefix(rest, "libpod/")
	return rest
}

// isVersionSegment reports whether s looks like a docker/podman API
// version path component: a leading 'v' followed by one or more
// dot-separated digit runs. "v1", "v1.41", "v5.0.0" all match;
// "version", "v", "v1a" do not.
func isVersionSegment(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	for _, c := range s[1:] {
		if (c < '0' || c > '9') && c != '.' {
			return false
		}
	}
	return true
}

// matchPath is a tiny pattern matcher: '+' matches exactly one
// non-empty path segment. There are no other meta-characters — this
// is not a regex engine, just a way to spell "containers/<id>/start"
// without inventing dozens of named-capture rules.
func matchPath(path, pattern string) bool {
	ps := strings.Split(path, "/")
	qs := strings.Split(pattern, "/")
	if len(ps) != len(qs) {
		return false
	}
	for i, q := range qs {
		if q == "+" {
			if ps[i] == "" {
				return false
			}
			continue
		}
		if q != ps[i] {
			return false
		}
	}
	return true
}

// classifyRequest applies the endpoint allowlist to the (method, normalised
// path) pair. The default-deny branch is the LAST return — every allow
// rule is an explicit positive match above it.
//
// The classification covers the docker / podman REST API surface that
// an agent legitimately needs for the workflows described in #2317 §1:
// container lifecycle (run / stop / rm), image pull, exec, build,
// network / volume create. Endpoints outside this surface are denied
// even if they are technically read-only — the proxy's job is to be
// narrow, not exhaustive.
//
// When this function returns endpointPolicyCreate or endpointPolicyArchive
// the caller MUST apply the corresponding body / query policy before
// forwarding. When it returns endpointAllowStreaming the caller MUST NOT
// attempt to read the body for inspection.
func classifyRequest(method, normPath string) endpointKind {
	if normPath == "" {
		return endpointDeny
	}

	switch method {
	case "GET":
		return classifyGET(normPath)
	case "HEAD":
		if normPath == "_ping" {
			return endpointAllow
		}
		return endpointDeny
	case "POST":
		return classifyPOST(normPath)
	case "PUT":
		return classifyPUT(normPath)
	case "DELETE":
		return classifyDELETE(normPath)
	}
	return endpointDeny
}

func classifyGET(normPath string) endpointKind {
	// Read-only system endpoints.
	switch normPath {
	case "_ping", "info", "version", "events", "system/df", "system/info":
		return endpointAllow
	case "containers/json", "images/json", "networks", "volumes",
		"images/search", "exec", "secrets", "configs":
		// Listing read-only.
		return endpointAllow
	}

	// Specific container read-paths.
	switch {
	case matchPath(normPath, "containers/+/json"),
		matchPath(normPath, "containers/+/stats"),
		matchPath(normPath, "containers/+/top"),
		matchPath(normPath, "containers/+/changes"),
		matchPath(normPath, "containers/+/export"),
		matchPath(normPath, "containers/+/archive"):
		return endpointAllow
	case matchPath(normPath, "containers/+/logs"):
		// /logs is GET-only and may stream when follow=1. The body
		// inspection path does not apply (there is no request body).
		return endpointAllowStreaming
	}

	// Image / network / volume / exec inspect.
	switch {
	case matchPath(normPath, "images/+/json"),
		matchPath(normPath, "images/+/history"),
		matchPath(normPath, "images/+/get"),
		matchPath(normPath, "networks/+"),
		matchPath(normPath, "volumes/+"),
		matchPath(normPath, "exec/+/json"):
		return endpointAllow
	}

	return endpointDeny
}

func classifyPOST(normPath string) endpointKind {
	// Body-inspected: this is the one and only endpoint where the
	// entire HostConfig policy is enforced.
	if normPath == "containers/create" {
		return endpointPolicyCreate
	}

	// Lifecycle ops on an existing container — no body fields can
	// re-introduce host binds or privileged execution.
	switch {
	case matchPath(normPath, "containers/+/start"),
		matchPath(normPath, "containers/+/stop"),
		matchPath(normPath, "containers/+/kill"),
		matchPath(normPath, "containers/+/restart"),
		matchPath(normPath, "containers/+/pause"),
		matchPath(normPath, "containers/+/unpause"),
		matchPath(normPath, "containers/+/wait"),
		matchPath(normPath, "containers/+/rename"),
		matchPath(normPath, "containers/+/resize"):
		return endpointAllow
	}

	// Streaming endpoints: no body parsing — see endpointAllowStreaming
	// comment for the rationale.
	switch {
	case matchPath(normPath, "containers/+/attach"),
		matchPath(normPath, "exec/+/start"):
		return endpointAllowStreaming
	}

	// Exec create has its own Privileged field that grants extra
	// capabilities to the exec process — body inspection is required
	// (review-security PR #2326 round 2). Exec resize is a small
	// tty-resize op with no security-relevant body.
	if matchPath(normPath, "containers/+/exec") {
		return endpointPolicyExec
	}
	if matchPath(normPath, "exec/+/resize") {
		return endpointAllow
	}

	// Prune ops.
	switch normPath {
	case "containers/prune", "images/prune", "networks/prune", "volumes/prune", "build/prune":
		return endpointAllow
	}

	// Image lifecycle.
	switch normPath {
	case "images/create", "images/load", "build", "commit":
		return endpointAllow
	}
	switch {
	case matchPath(normPath, "images/+/tag"),
		matchPath(normPath, "images/+/push"):
		return endpointAllow
	}

	// Network and volume create / connect / disconnect.
	switch normPath {
	case "networks/create", "volumes/create":
		return endpointAllow
	}
	switch {
	case matchPath(normPath, "networks/+/connect"),
		matchPath(normPath, "networks/+/disconnect"):
		return endpointAllow
	}

	// Container update accepts Memory, CpuQuota, NanoCpus, etc. — an
	// agent that creates with valid caps could otherwise POST update
	// with Memory=0 to remove the cap (review-security PR #2326 round 2).
	// Body inspection is required.
	if matchPath(normPath, "containers/+/update") {
		return endpointPolicyUpdate
	}

	return endpointDeny
}

func classifyPUT(normPath string) endpointKind {
	if matchPath(normPath, "containers/+/archive") {
		return endpointPolicyArchive
	}
	return endpointDeny
}

func classifyDELETE(normPath string) endpointKind {
	switch {
	case matchPath(normPath, "containers/+"),
		matchPath(normPath, "images/+"),
		matchPath(normPath, "networks/+"),
		matchPath(normPath, "volumes/+"),
		matchPath(normPath, "exec/+"):
		return endpointAllow
	}
	return endpointDeny
}
