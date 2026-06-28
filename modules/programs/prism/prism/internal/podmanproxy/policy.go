package podmanproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
)

// policyDecision is the outcome of inspecting a request body or query.
// allow is the only field that gates forwarding; status and reason are
// only consumed when allow is false.
type policyDecision struct {
	allow  bool
	status int    // HTTP status code to return when allow is false
	reason string // structured reason string written to the audit log
	// message is the human-readable text embedded in the JSON envelope
	// returned to the client. When empty, reason is reused.
	message string
}

// allowDecision returns a "forward upstream" decision with the supplied
// audit reason. Helper to keep policy callers readable.
func allowDecision(reason string) policyDecision {
	return policyDecision{allow: true, reason: reason}
}

// denyDecision returns a deny decision. The message is the friendly
// text shown to the client; the reason is the structured audit token.
func denyDecision(status int, reason, message string) policyDecision {
	if message == "" {
		message = reason
	}
	return policyDecision{
		allow:   false,
		status:  status,
		reason:  reason,
		message: message,
	}
}

// hostConfig is the subset of HostConfig the proxy parses out of
// containers/create. Every field listed in #2317 §4 (the threat table)
// must appear here; fields not present in this struct are silently
// ignored by the parser (json.Decoder skips unknown keys by default).
//
// Pointer types are used where the difference between "field absent"
// and "field explicitly set to zero/false" matters — Memory and
// CpuQuota in particular, where an explicit 0 means "unbounded" in
// docker's semantics and should NOT be capped.
type hostConfig struct {
	Binds       []string          `json:"Binds"`
	Mounts      []hostConfigMount `json:"Mounts"`
	Privileged  bool              `json:"Privileged"`
	CapAdd      []string          `json:"CapAdd"`
	NetworkMode string            `json:"NetworkMode"`
	PidMode     string            `json:"PidMode"`
	IpcMode     string            `json:"IpcMode"`
	UTSMode     string            `json:"UTSMode"`
	UsernsMode  string            `json:"UsernsMode"`
	Devices     []json.RawMessage `json:"Devices"`
	Memory      *int64            `json:"Memory"`
	CpuQuota    *int64            `json:"CpuQuota"`
}

// hostConfigMount mirrors a docker HostConfig.Mounts entry. Only the
// fields needed for the bind-source check are extracted.
type hostConfigMount struct {
	Type   string `json:"Type"`
	Source string `json:"Source"`
}

// containerCreateBody is the top-level shape of POST containers/create.
// We extract only HostConfig — the other fields (Image, Cmd, Env, Labels,
// WorkingDir, NetworkingConfig, etc.) carry no policy concerns and are
// forwarded unmodified.
type containerCreateBody struct {
	HostConfig *hostConfig `json:"HostConfig"`
}

// inspectCreate parses body as a containers/create request and applies
// the HostConfig policy. body must be the entire request body; on
// success the caller restores r.Body before forwarding.
func (p *Proxy) inspectCreate(body []byte) policyDecision {
	if len(bytes.TrimSpace(body)) == 0 {
		// An empty body is malformed for containers/create — even a
		// minimal request needs at least {"Image":"..."}. Per the
		// "malformed JSON returns 400" AC, deny.
		return denyDecision(http.StatusBadRequest,
			"malformed_body:empty",
			"containers/create request body is empty")
	}

	var req containerCreateBody
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&req); err != nil {
		return denyDecision(http.StatusBadRequest,
			"malformed_body:"+truncateForReason(err.Error()),
			"containers/create body is not valid JSON")
	}

	if req.HostConfig == nil {
		// No HostConfig is harmless — a container without any host
		// configuration cannot bind-mount or request privileges.
		return allowDecision("policy:containers/create:no_host_config")
	}

	return p.checkHostConfig(req.HostConfig)
}

// checkHostConfig walks the parsed HostConfig and returns the first
// policy violation. Field order in this function is significant: the
// most dangerous fields (host binds, privileged) are checked first so
// the audit reason names the worst violation when multiple are present.
func (p *Proxy) checkHostConfig(hc *hostConfig) policyDecision {
	// Host bind sources (legacy Binds slice).
	for _, b := range hc.Binds {
		src := bindSource(b)
		if src == "" {
			// A volume name (no leading '/') is not a host bind.
			continue
		}
		if !p.isAllowedBindSource(src) {
			return denyDecision(http.StatusForbidden,
				"host_bind:"+truncateForReason(src),
				fmt.Sprintf("host bind source %q is not in the allowlist", src))
		}
	}

	// Host bind sources (newer Mounts slice).
	for _, m := range hc.Mounts {
		if !strings.EqualFold(m.Type, "bind") {
			// "volume", "tmpfs", "npipe", "image" do not read host
			// files. (npipe is Windows-only; we let the upstream
			// reject it on Linux/Darwin.)
			continue
		}
		if !p.isAllowedBindSource(m.Source) {
			return denyDecision(http.StatusForbidden,
				"mount_bind:"+truncateForReason(m.Source),
				fmt.Sprintf("mount source %q is not in the allowlist", m.Source))
		}
	}

	// Privileged is a single bit. Any "true" is a hard reject.
	if hc.Privileged {
		return denyDecision(http.StatusForbidden,
			"privileged",
			"HostConfig.Privileged is not permitted")
	}

	// CapAdd — every entry must be in the allowlist. CapDrop is
	// unrestricted (dropping capabilities is always safer).
	if len(hc.CapAdd) > 0 {
		if cap, ok := p.firstDisallowedCap(hc.CapAdd); !ok {
			return denyDecision(http.StatusForbidden,
				"cap_add:"+truncateForReason(cap),
				fmt.Sprintf("HostConfig.CapAdd entry %q is not in the allowlist", cap))
		}
	}

	// Host namespaces — any *Mode = "host" is a hard reject.
	if mode := strings.ToLower(hc.NetworkMode); mode == "host" {
		return denyDecision(http.StatusForbidden,
			"network_mode_host",
			"HostConfig.NetworkMode=host is not permitted")
	}
	if mode := strings.ToLower(hc.PidMode); mode == "host" {
		return denyDecision(http.StatusForbidden,
			"pid_mode_host",
			"HostConfig.PidMode=host is not permitted")
	}
	if mode := strings.ToLower(hc.IpcMode); mode == "host" {
		return denyDecision(http.StatusForbidden,
			"ipc_mode_host",
			"HostConfig.IpcMode=host is not permitted")
	}
	if mode := strings.ToLower(hc.UTSMode); mode == "host" {
		return denyDecision(http.StatusForbidden,
			"uts_mode_host",
			"HostConfig.UTSMode=host is not permitted")
	}
	if mode := strings.ToLower(hc.UsernsMode); mode == "host" {
		return denyDecision(http.StatusForbidden,
			"userns_mode_host",
			"HostConfig.UsernsMode=host is not permitted")
	}

	// Device passthrough — any non-empty Devices is a hard reject.
	// A future enhancement could allowlist e.g. /dev/null; for now
	// we are strictly default-deny.
	if len(hc.Devices) > 0 {
		return denyDecision(http.StatusForbidden,
			"devices_nonempty",
			"HostConfig.Devices is not permitted")
	}

	// Resource caps. Memory=0 in docker means "unlimited" — but our
	// policy treats it as "no value set" and lets it through. The
	// upper bound only fires when the agent explicitly requested a
	// value above the cap.
	if p.cfg.MaxMemoryBytes > 0 && hc.Memory != nil && *hc.Memory > p.cfg.MaxMemoryBytes {
		return denyDecision(http.StatusForbidden,
			"memory_over_cap",
			fmt.Sprintf("HostConfig.Memory=%d exceeds cap %d", *hc.Memory, p.cfg.MaxMemoryBytes))
	}
	if p.cfg.MaxCPUQuota > 0 && hc.CpuQuota != nil && *hc.CpuQuota > p.cfg.MaxCPUQuota {
		return denyDecision(http.StatusForbidden,
			"cpu_quota_over_cap",
			fmt.Sprintf("HostConfig.CpuQuota=%d exceeds cap %d", *hc.CpuQuota, p.cfg.MaxCPUQuota))
	}

	return allowDecision("policy:containers/create:ok")
}

// inspectArchive applies the path-prefix policy to PUT
// containers/{id}/archive. The dangerous field is the `path` query
// parameter — the in-container destination. Defence-in-depth: even
// though Binds policy already filters mount sources, restricting the
// archive write path stops an agent that finds a container with a
// system mount from using `podman cp` to clobber host files.
func (p *Proxy) inspectArchive(r *http.Request) policyDecision {
	path := r.URL.Query().Get("path")
	if path == "" {
		return denyDecision(http.StatusBadRequest,
			"archive_missing_path",
			"PUT /containers/{id}/archive requires a non-empty `path` query parameter")
	}
	if !p.isAllowedBindSource(path) {
		return denyDecision(http.StatusForbidden,
			"archive_path:"+truncateForReason(path),
			fmt.Sprintf("archive path %q is not in the allowlist", path))
	}
	return allowDecision("policy:containers/archive:ok")
}

// bindSource extracts the host source from a HostConfig.Binds entry of
// the form "src:dst[:options]". If the source has no leading '/' it is
// treated as a named volume and bindSource returns "" so the caller
// skips the host-path check.
//
// Edge cases:
//   - "src::ro" (empty dst) — still extract src; the upstream will
//     reject; we err on the side of inspecting the source anyway.
//   - "src" alone (no colon) — invalid bind syntax; return "" so the
//     upstream returns its own malformed-bind error and we don't
//     fabricate a security decision for something podman will reject.
func bindSource(bind string) string {
	idx := strings.Index(bind, ":")
	if idx < 0 {
		return ""
	}
	src := bind[:idx]
	if !strings.HasPrefix(src, "/") {
		// Named volume, not a host path.
		return ""
	}
	return src
}

// isAllowedBindSource reports whether src is permitted as a host bind
// source given the configured prefix allowlist. A path is allowed iff,
// after filepath.Clean, it equals an entry exactly or starts with
// entry + "/". The substring trap — "/srv" allowing "/srv-other" — is
// closed by requiring the separator.
//
// The special entry "/" allows every absolute path; it exists so the
// security test suite can run a negative-control case proving the
// positive tests are not no-ops.
func (p *Proxy) isAllowedBindSource(src string) bool {
	if src == "" {
		return false
	}
	if !strings.HasPrefix(src, "/") {
		return false
	}
	clean := filepath.Clean(src)
	for _, raw := range p.cfg.AllowedBindSources {
		if raw == "" {
			continue
		}
		allowed := filepath.Clean(raw)
		if allowed == "/" {
			return true
		}
		if clean == allowed {
			return true
		}
		if strings.HasPrefix(clean, allowed+"/") {
			return true
		}
	}
	return false
}

// firstDisallowedCap returns the first CapAdd entry that is not in the
// allowlist, or ("", true) if every entry is permitted. The comparison
// is case-insensitive and tolerant of the leading "CAP_" prefix on
// either side so callers can pass either form.
func (p *Proxy) firstDisallowedCap(caps []string) (string, bool) {
	for _, c := range caps {
		if !p.capIsAllowed(c) {
			return c, false
		}
	}
	return "", true
}

func (p *Proxy) capIsAllowed(c string) bool {
	want := strings.ToUpper(strings.TrimPrefix(strings.ToUpper(c), "CAP_"))
	for _, allowed := range p.cfg.AllowedCaps {
		got := strings.ToUpper(strings.TrimPrefix(strings.ToUpper(allowed), "CAP_"))
		if want == got {
			return true
		}
	}
	return false
}

// truncateForReason bounds a free-form string before splicing it into
// the audit reason field. Bind sources and error messages can be
// arbitrarily long; the audit log should not blow up to MiBs per line
// because the attacker padded a path.
func truncateForReason(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// readBoundedBody reads the request body into memory subject to
// p.cfg.MaxBodyBytes. It returns a decision when the body exceeds the
// cap or another read error occurs; on success it returns the body
// bytes and an allow decision so the caller can proceed.
//
// The error wrapping here uses *http.MaxBytesError so the caller can
// distinguish "too large" (413) from other I/O failures (400).
func (p *Proxy) readBoundedBody(w http.ResponseWriter, r *http.Request) ([]byte, policyDecision) {
	reader := http.MaxBytesReader(w, r.Body, p.cfg.MaxBodyBytes)
	defer r.Body.Close()
	body, err := io.ReadAll(reader)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, denyDecision(http.StatusRequestEntityTooLarge,
				"body_too_large",
				fmt.Sprintf("request body exceeds %d bytes", p.cfg.MaxBodyBytes))
		}
		return nil, denyDecision(http.StatusBadRequest,
			"body_read_error",
			"could not read request body")
	}
	return body, allowDecision("body_read_ok")
}
