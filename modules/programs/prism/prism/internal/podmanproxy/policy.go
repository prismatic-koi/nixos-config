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
// containers/create (and the partial-HostConfig shape that
// containers/{id}/update also accepts). Every field listed in #2317 §4
// (the threat table) must appear here. Fields not present in this
// struct are silently ignored by the parser (json.Decoder skips
// unknown keys by default) and forwarded unmodified.
//
// Pointer types are used where the difference between "field absent"
// and "field explicitly set to zero" matters — Memory, CpuQuota, and
// NanoCpus in particular. In docker semantics an explicit 0 means
// "unbounded", so the policy must treat 0 differently from a missing
// field (review-security PR #2326 round 2: 0 was previously a
// drive-through bypass of the configured cap).
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
	SecurityOpt []string          `json:"SecurityOpt"`
	Memory      *int64            `json:"Memory"`
	CpuQuota    *int64            `json:"CpuQuota"`
	NanoCpus    *int64            `json:"NanoCpus"`
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

// containerExecBody is the partial shape of POST containers/{id}/exec.
// The only field we inspect is Privileged: docker exec grants the
// supplied capability set to the exec process independent of the
// parent container's HostConfig.Privileged, so an agent that creates
// a non-privileged container can otherwise exec into it with
// Privileged: true and bypass the create-time deny. Cmd, Env, User,
// WorkingDir, etc. are forwarded unmodified — they affect what runs
// inside the (already-isolated) container, not the host.
type containerExecBody struct {
	Privileged bool `json:"Privileged"`
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

	// SecurityOpt entries are default-deny: any entry not present in
	// AllowedSecurityOpts is rejected. This closes the
	// seccomp=unconfined / apparmor=unconfined / no-new-privileges=false /
	// label=disable class of escapes; allowlist entries one by one when
	// a workflow genuinely needs them.
	if bad, ok := p.firstDisallowedSecurityOpt(hc.SecurityOpt); !ok {
		return denyDecision(http.StatusForbidden,
			"security_opt:"+truncateForReason(bad),
			fmt.Sprintf("HostConfig.SecurityOpt entry %q is not in the allowlist", bad))
	}

	// Resource caps. Strict mode: when a cap is configured the
	// corresponding field MUST be set to a positive value within the
	// cap. Absent / zero / negative all deny so that docker's "0 means
	// unlimited" semantic cannot be used to bypass the cap.
	if dec := p.checkResourceCaps(hc, capContextCreate); !dec.allow {
		return dec
	}

	return allowDecision("policy:containers/create:ok")
}

// capContext distinguishes the two body shapes that resource-cap
// checks apply to. The semantics differ in one place: on create, an
// absent field is a deny (the agent must declare an explicit value
// within the cap); on update, an absent field is allowed (the field
// is simply not being updated).
type capContext int

const (
	capContextCreate capContext = iota
	capContextUpdate
)

// checkResourceCaps validates Memory, CpuQuota, and NanoCpus against
// the configured caps. Called from both the containers/create body
// inspector and the containers/{id}/update body inspector; the ctx
// flag toggles the absent-field policy between the two.
func (p *Proxy) checkResourceCaps(hc *hostConfig, ctx capContext) policyDecision {
	if dec := p.checkOneResourceCap(
		"Memory", hc.Memory, p.cfg.MaxMemoryBytes, ctx,
		"memory_required", "memory_nonpositive", "memory_over_cap",
	); !dec.allow {
		return dec
	}
	if dec := p.checkOneResourceCap(
		"CpuQuota", hc.CpuQuota, p.cfg.MaxCPUQuota, ctx,
		"cpu_quota_required", "cpu_quota_nonpositive", "cpu_quota_over_cap",
	); !dec.allow {
		return dec
	}
	if dec := p.checkOneResourceCap(
		"NanoCpus", hc.NanoCpus, p.cfg.MaxNanoCpus, ctx,
		"nano_cpus_required", "nano_cpus_nonpositive", "nano_cpus_over_cap",
	); !dec.allow {
		return dec
	}
	return allowDecision("policy:resource_caps:ok")
}

// checkOneResourceCap is the per-field cap checker. The three reason
// strings are passed in so the audit log distinguishes which cap
// fired and why — "memory_required" vs "memory_nonpositive" vs
// "memory_over_cap".
func (p *Proxy) checkOneResourceCap(
	fieldName string, value *int64, cap int64, ctx capContext,
	reasonRequired, reasonNonpositive, reasonOverCap string,
) policyDecision {
	if cap <= 0 {
		// Cap not configured — no enforcement.
		return allowDecision("policy:resource_cap_disabled")
	}
	if value == nil {
		if ctx == capContextCreate {
			return denyDecision(http.StatusForbidden,
				reasonRequired,
				fmt.Sprintf("HostConfig.%s is required when a cap is configured (set a positive value <= %d)", fieldName, cap))
		}
		// Update: absent field means "not changing this". Allow.
		return allowDecision("policy:resource_cap_absent_in_update")
	}
	if *value <= 0 {
		return denyDecision(http.StatusForbidden,
			reasonNonpositive,
			fmt.Sprintf("HostConfig.%s=%d is invalid (must be > 0; 0 means unlimited and would bypass the cap)", fieldName, *value))
	}
	if *value > cap {
		return denyDecision(http.StatusForbidden,
			reasonOverCap,
			fmt.Sprintf("HostConfig.%s=%d exceeds cap %d", fieldName, *value, cap))
	}
	return allowDecision("policy:resource_cap_ok")
}

// inspectUpdate parses body as a containers/{id}/update request and
// applies the resource-cap policy. Update bodies are a partial
// HostConfig shape — only the fields being changed are present — so
// the cap check runs in update-context mode (absent fields allowed,
// present fields must be within bounds).
//
// This closes the bypass where an agent creates a container with
// Memory=4G (within cap) and then POSTs an update with Memory=0 to
// remove the cap (#2326 round 2 review-security).
func (p *Proxy) inspectUpdate(body []byte) policyDecision {
	if len(bytes.TrimSpace(body)) == 0 {
		// An empty update body is a no-op upstream — forward it. The
		// upstream will return 200 with no state change.
		return allowDecision("policy:containers/update:empty")
	}
	var hc hostConfig
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&hc); err != nil {
		return denyDecision(http.StatusBadRequest,
			"malformed_body:"+truncateForReason(err.Error()),
			"containers/update body is not valid JSON")
	}
	return p.checkResourceCaps(&hc, capContextUpdate)
}

// inspectExec parses body as a containers/{id}/exec request and
// rejects Privileged: true. The exec body has its own Privileged
// field that grants additional capabilities to the exec process
// independent of the parent container's HostConfig.Privileged — the
// create-time deny does not cover it.
func (p *Proxy) inspectExec(body []byte) policyDecision {
	if len(bytes.TrimSpace(body)) == 0 {
		// An exec body of {} is valid (defaults). An empty body is
		// arguable but harmless — forward and let the upstream decide.
		return allowDecision("policy:containers/exec:empty")
	}
	var req containerExecBody
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&req); err != nil {
		return denyDecision(http.StatusBadRequest,
			"malformed_body:"+truncateForReason(err.Error()),
			"containers/exec body is not valid JSON")
	}
	if req.Privileged {
		return denyDecision(http.StatusForbidden,
			"exec_privileged",
			"containers/exec body has Privileged=true, which is not permitted (would bypass the create-time HostConfig.Privileged deny)")
	}
	return allowDecision("policy:containers/exec:ok")
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

// firstDisallowedSecurityOpt returns the first SecurityOpt entry that
// is not in AllowedSecurityOpts. Comparison is exact (case-sensitive,
// no normalisation) because SecurityOpt values are docker-defined
// strings whose exact form matters — "no-new-privileges:true" and
// "no-new-privileges=true" are both valid but distinct, and an
// allowlist entry should match exactly what the caller permits.
func (p *Proxy) firstDisallowedSecurityOpt(opts []string) (string, bool) {
	for _, o := range opts {
		if !p.securityOptIsAllowed(o) {
			return o, false
		}
	}
	return "", true
}

func (p *Proxy) securityOptIsAllowed(opt string) bool {
	for _, allowed := range p.cfg.AllowedSecurityOpts {
		if opt == allowed {
			return true
		}
	}
	return false
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
