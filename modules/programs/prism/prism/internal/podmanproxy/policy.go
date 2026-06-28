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
// Cycle 5: this struct is the AUDIT SPEC for HostConfig. After this
// commit the parser runs with json.Decoder.DisallowUnknownFields(),
// so any HostConfig field not declared here is rejected as
// unknown-field. Adding a new field requires the same audit as
// existing ones: classify it as INSPECTED (typed; checkHostConfig
// runs a policy check) / DENIED (typed; non-empty rejects) /
// FORWARDED (json.RawMessage; admitted as safe, forwarded
// unmodified). The single most important reviewer task on a future
// change to this file is checking the rationale comment on each
// admission.
type hostConfig struct {
	// INSPECTED — policy check in checkHostConfig.
	Binds        []string          `json:"Binds"`        // bind allowlist + symlink resolution
	Mounts       []hostConfigMount `json:"Mounts"`       // bind allowlist + volume-driver escape
	Privileged   bool              `json:"Privileged"`   // deny when true
	CapAdd       []string          `json:"CapAdd"`       // allowlist (AllowedCaps)
	NetworkMode  string            `json:"NetworkMode"`  // deny "host"
	PidMode      string            `json:"PidMode"`      // deny "host"
	IpcMode      string            `json:"IpcMode"`      // deny "host"
	UTSMode      string            `json:"UTSMode"`      // deny "host"
	UsernsMode   string            `json:"UsernsMode"`   // deny "host"
	CgroupnsMode string            `json:"CgroupnsMode"` // deny "host"
	SecurityOpt  []string          `json:"SecurityOpt"`  // allowlist (AllowedSecurityOpts)
	Memory       *int64            `json:"Memory"`       // cap; strict when MaxMemoryBytes>0
	CpuQuota     *int64            `json:"CpuQuota"`     // cap; strict when MaxCPUQuota>0
	NanoCpus     *int64            `json:"NanoCpus"`     // cap; strict when MaxNanoCpus>0

	// DENIED — typed; non-empty / non-default value rejects.
	Devices           []json.RawMessage `json:"Devices"`           // deny non-empty
	DeviceCgroupRules []string          `json:"DeviceCgroupRules"` // deny non-empty
	DeviceRequests    []json.RawMessage `json:"DeviceRequests"`    // deny non-empty
	VolumesFrom       []string          `json:"VolumesFrom"`       // deny non-empty
	MaskedPaths       *[]string         `json:"MaskedPaths"`       // deny when present
	ReadonlyPaths     *[]string         `json:"ReadonlyPaths"`     // deny when present
	Sysctls           map[string]string `json:"Sysctls"`           // deny non-empty

	// FORWARDED — admitted as safe; bytes forwarded unmodified.
	// Each entry below MUST have a rationale comment confirming it
	// has no escape vector against the parent issue's threat table.
	CapDrop              json.RawMessage `json:"CapDrop"`              // dropping caps is always safer
	AutoRemove           json.RawMessage `json:"AutoRemove"`           // container lifecycle
	RestartPolicy        json.RawMessage `json:"RestartPolicy"`        // container lifecycle
	LogConfig            json.RawMessage `json:"LogConfig"`            // log driver settings
	Tmpfs                json.RawMessage `json:"Tmpfs"`                // in-memory, container-internal
	PortBindings         json.RawMessage `json:"PortBindings"`         // network port map
	PublishAllPorts      json.RawMessage `json:"PublishAllPorts"`      // network port flag
	ReadonlyRootfs       json.RawMessage `json:"ReadonlyRootfs"`       // safer-default; not escape
	ExtraHosts           json.RawMessage `json:"ExtraHosts"`           // /etc/hosts entries
	GroupAdd             json.RawMessage `json:"GroupAdd"`             // additional gids inside ct
	Dns                  json.RawMessage `json:"Dns"`                  // DNS servers
	DnsOptions           json.RawMessage `json:"DnsOptions"`           // DNS resolver options
	DnsSearch            json.RawMessage `json:"DnsSearch"`            // DNS search domains
	Links                json.RawMessage `json:"Links"`                // deprecated container-to-container
	Cgroup               json.RawMessage `json:"Cgroup"`               // cgroup name (not host control)
	CgroupParent         json.RawMessage `json:"CgroupParent"`         // parent cgroup placement
	BlkioWeight          json.RawMessage `json:"BlkioWeight"`          // I/O QoS
	BlkioWeightDevice    json.RawMessage `json:"BlkioWeightDevice"`    // I/O QoS per-device
	BlkioDeviceReadBps   json.RawMessage `json:"BlkioDeviceReadBps"`   // I/O QoS
	BlkioDeviceWriteBps  json.RawMessage `json:"BlkioDeviceWriteBps"`  // I/O QoS
	BlkioDeviceReadIOps  json.RawMessage `json:"BlkioDeviceReadIOps"`  // I/O QoS
	BlkioDeviceWriteIOps json.RawMessage `json:"BlkioDeviceWriteIOps"` // I/O QoS
	CpuShares            json.RawMessage `json:"CpuShares"`            // CPU QoS (relative weighting)
	CpuPeriod            json.RawMessage `json:"CpuPeriod"`            // CFS period (paired with CpuQuota)
	CpuRealtimePeriod    json.RawMessage `json:"CpuRealtimePeriod"`    // RT QoS
	CpuRealtimeRuntime   json.RawMessage `json:"CpuRealtimeRuntime"`   // RT QoS
	CpusetCpus           json.RawMessage `json:"CpusetCpus"`           // CPU pinning
	CpusetMems           json.RawMessage `json:"CpusetMems"`           // NUMA pinning
	CpuPercent           json.RawMessage `json:"CpuPercent"`           // windows CPU %
	CpuCount             json.RawMessage `json:"CpuCount"`             // windows CPU count
	IOMaximumIOps        json.RawMessage `json:"IOMaximumIOps"`        // windows I/O cap
	IOMaximumBandwidth   json.RawMessage `json:"IOMaximumBandwidth"`   // windows I/O cap
	MemoryReservation    json.RawMessage `json:"MemoryReservation"`    // soft memory limit
	MemorySwap           json.RawMessage `json:"MemorySwap"`           // swap size cap
	MemorySwappiness     json.RawMessage `json:"MemorySwappiness"`     // swap tendency
	KernelMemory         json.RawMessage `json:"KernelMemory"`         // deprecated kernel mem
	KernelMemoryTCP      json.RawMessage `json:"KernelMemoryTCP"`      // kernel TCP buffer cap
	OomKillDisable       json.RawMessage `json:"OomKillDisable"`       // OOM behaviour
	OomScoreAdj          json.RawMessage `json:"OomScoreAdj"`          // OOM score bias
	PidsLimit            json.RawMessage `json:"PidsLimit"`            // ct process count cap
	Ulimits              json.RawMessage `json:"Ulimits"`              // per-ct rlimits
	StorageOpt           json.RawMessage `json:"StorageOpt"`           // storage driver opts
	ContainerIDFile      json.RawMessage `json:"ContainerIDFile"`      // path to write ct id
	Init                 json.RawMessage `json:"Init"`                 // pid 1 init wrapper
	VolumeDriver         json.RawMessage `json:"VolumeDriver"`         // default volume driver name
	ConsoleSize          json.RawMessage `json:"ConsoleSize"`          // tty size
	Annotations          json.RawMessage `json:"Annotations"`          // OCI annotations
	DiskQuota            json.RawMessage `json:"DiskQuota"`            // disk quota
	Isolation            json.RawMessage `json:"Isolation"`            // windows isolation
	NetworkID            json.RawMessage `json:"NetworkID"`            // podman network id
	ShmSize              json.RawMessage `json:"ShmSize"`              // /dev/shm size
	Runtime              json.RawMessage `json:"Runtime"`              // runtime name (runc/crun)
}

// hostConfigMount mirrors a docker HostConfig.Mounts entry. The
// VolumeOptions sub-struct is parsed so we can deny the local-driver
// bind-volume escape (#2326 round 4 review-security CRITICAL #1):
// Type=volume with VolumeOptions.DriverConfig.Name="local" plus
// DriverConfig.Options.device=/host/path is functionally a bind mount
// dressed up as a volume.
type hostConfigMount struct {
	Type          string                   `json:"Type"`          // INSPECTED (bind vs volume)
	Source        string                   `json:"Source"`        // INSPECTED (host bind path)
	VolumeOptions *hostConfigVolumeOptions `json:"VolumeOptions"` // INSPECTED (deny .DriverConfig)

	// FORWARDED — mount fields admitted as safe.
	Target         json.RawMessage `json:"Target"`         // in-container path
	ReadOnly       json.RawMessage `json:"ReadOnly"`       // safer-default
	Consistency    json.RawMessage `json:"Consistency"`    // macOS perf flag
	BindOptions    json.RawMessage `json:"BindOptions"`    // propagation flags
	TmpfsOptions   json.RawMessage `json:"TmpfsOptions"`   // size/mode for tmpfs Type
	ClusterOptions json.RawMessage `json:"ClusterOptions"` // swarm cluster volumes
}

type hostConfigVolumeOptions struct {
	DriverConfig *hostConfigDriverConfig `json:"DriverConfig"` // INSPECTED — presence denies

	// FORWARDED.
	NoCopy  json.RawMessage `json:"NoCopy"`
	Labels  json.RawMessage `json:"Labels"`
	Subpath json.RawMessage `json:"Subpath"`
}

type hostConfigDriverConfig struct {
	Name    string            `json:"Name"`
	Options map[string]string `json:"Options"`
}

// containerCreateBody is the top-level shape of POST containers/create.
// Same allow-list discipline as hostConfig: every field admitted here
// is either INSPECTED, DENIED, or FORWARDED with a rationale.
//
// Top-level fields are largely container-internal (Image, Cmd, Env,
// WorkingDir, Labels, etc.) and have no documented escape vector.
// HostConfig and NetworkingConfig are nested structures — only
// HostConfig has a parser; NetworkingConfig is admitted as opaque
// for now and revisited if it surfaces escapes.
type containerCreateBody struct {
	// INSPECTED.
	HostConfig *json.RawMessage `json:"HostConfig"` // parsed strictly in a second pass

	// FORWARDED — container-internal config; no host-impact.
	Hostname         json.RawMessage `json:"Hostname"`
	Domainname       json.RawMessage `json:"Domainname"`
	User             json.RawMessage `json:"User"`
	AttachStdin      json.RawMessage `json:"AttachStdin"`
	AttachStdout     json.RawMessage `json:"AttachStdout"`
	AttachStderr     json.RawMessage `json:"AttachStderr"`
	ExposedPorts     json.RawMessage `json:"ExposedPorts"`
	Tty              json.RawMessage `json:"Tty"`
	OpenStdin        json.RawMessage `json:"OpenStdin"`
	StdinOnce        json.RawMessage `json:"StdinOnce"`
	Env              json.RawMessage `json:"Env"`
	Cmd              json.RawMessage `json:"Cmd"`
	Healthcheck      json.RawMessage `json:"Healthcheck"`
	ArgsEscaped      json.RawMessage `json:"ArgsEscaped"` // windows
	Image            json.RawMessage `json:"Image"`
	Volumes          json.RawMessage `json:"Volumes"` // anonymous-volume placeholders
	WorkingDir       json.RawMessage `json:"WorkingDir"`
	Entrypoint       json.RawMessage `json:"Entrypoint"`
	NetworkDisabled  json.RawMessage `json:"NetworkDisabled"`
	MacAddress       json.RawMessage `json:"MacAddress"`
	OnBuild          json.RawMessage `json:"OnBuild"`
	Labels           json.RawMessage `json:"Labels"`
	StopSignal       json.RawMessage `json:"StopSignal"`
	StopTimeout      json.RawMessage `json:"StopTimeout"`
	Shell            json.RawMessage `json:"Shell"`
	NetworkingConfig json.RawMessage `json:"NetworkingConfig"`
	// podman libpod additions
	Name json.RawMessage `json:"Name"` // libpod allows Name in body (in addition to ?name=)
}

// containerExecBody is the explicit allowlist for POST
// containers/{id}/exec. Privileged is INSPECTED (cycle-2 bypass
// fix); everything else is admitted as opaque — the exec body
// describes what to run INSIDE the (already-isolated) container.
type containerExecBody struct {
	// INSPECTED.
	Privileged bool `json:"Privileged"`

	// FORWARDED.
	AttachStdin  json.RawMessage `json:"AttachStdin"`
	AttachStdout json.RawMessage `json:"AttachStdout"`
	AttachStderr json.RawMessage `json:"AttachStderr"`
	DetachKeys   json.RawMessage `json:"DetachKeys"`
	Tty          json.RawMessage `json:"Tty"`
	Env          json.RawMessage `json:"Env"`
	Cmd          json.RawMessage `json:"Cmd"`
	User         json.RawMessage `json:"User"`
	WorkingDir   json.RawMessage `json:"WorkingDir"`
	ConsoleSize  json.RawMessage `json:"ConsoleSize"`
}

// inspectCreate parses body as a containers/create request and
// applies the HostConfig policy. Two-stage parse: first the
// top-level body with DisallowUnknownFields (rejects unknown
// top-level fields), then — if HostConfig is present — the
// HostConfig with DisallowUnknownFields too.
//
// The two-stage parse exists because the JSON for HostConfig is
// nested. A flat single-pass parser would accept ANY shape inside
// HostConfig if the top-level admits HostConfig as RawMessage. The
// second pass is where the per-field allowlist actually fires.
func (p *Proxy) inspectCreate(body []byte) policyDecision {
	if len(bytes.TrimSpace(body)) == 0 {
		return denyDecision(http.StatusBadRequest,
			"malformed_body:empty",
			"containers/create request body is empty")
	}

	var req containerCreateBody
	if dec := decodeStrict(body, &req); !dec.allow {
		dec.reason = "create_top:" + dec.reason
		dec.message = "containers/create top-level body: " + dec.message
		return dec
	}

	if req.HostConfig == nil || len(*req.HostConfig) == 0 {
		// No HostConfig is harmless — a container without any host
		// configuration cannot bind-mount or request privileges.
		return allowDecision("policy:containers/create:no_host_config")
	}

	var hc hostConfig
	if dec := decodeStrict(*req.HostConfig, &hc); !dec.allow {
		dec.reason = "create_hostconfig:" + dec.reason
		dec.message = "containers/create HostConfig: " + dec.message
		return dec
	}

	return p.checkHostConfig(&hc)
}

// decodeStrict runs json.Decoder.DisallowUnknownFields against body
// and returns a uniform policyDecision distinguishing
// unknown-field (the schema-inversion deny we WANT to surface
// loudly) from malformed-JSON (the original cycle-2 "400 on bad
// body" path). The reason and message strings are general-purpose;
// callers prefix them with an endpoint-specific tag so the audit
// log makes the source endpoint clear.
func decodeStrict(body []byte, dst any) policyDecision {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "unknown field") {
			return denyDecision(http.StatusForbidden,
				"unknown_field:"+truncateForReason(msg),
				"body contains a field not in the proxy's allowlist ("+truncateForReason(msg)+")")
		}
		return denyDecision(http.StatusBadRequest,
			"malformed_body:"+truncateForReason(msg),
			"body is not valid JSON")
	}
	return allowDecision("decode_ok")
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
		if strings.EqualFold(m.Type, "bind") {
			if !p.isAllowedBindSource(m.Source) {
				return denyDecision(http.StatusForbidden,
					"mount_bind:"+truncateForReason(m.Source),
					fmt.Sprintf("mount source %q is not in the allowlist", m.Source))
			}
			continue
		}
		if strings.EqualFold(m.Type, "volume") {
			// Type=volume with an inline DriverConfig is the
			// local-driver bind-volume escape: VolumeOptions.
			// DriverConfig.{Name,Options} can specify a host path
			// to bind-mount via the volume mechanism, bypassing the
			// Type=bind allowlist check above. The conservative fix
			// per the cycle-5 coordinator directive is to deny any
			// VolumeOptions.DriverConfig at all — the legitimate
			// "named volume managed by podman" case has no
			// DriverConfig (uses the default driver settings).
			if m.VolumeOptions != nil && m.VolumeOptions.DriverConfig != nil {
				return denyDecision(http.StatusForbidden,
					"mount_volume_driver_config",
					"Mounts entry of Type=volume with VolumeOptions.DriverConfig is not permitted (local-driver bind-volume escape; use a Type=bind Mount with an allowlisted Source instead)")
			}
			continue
		}
		// "tmpfs" (in-memory, container-internal), "npipe"
		// (Windows), "image" (image-volume): no host-file access
		// path, forward unmodified.
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

	// Host namespaces — any *Mode = "host" is a hard reject. The
	// CgroupnsMode entry is the cycle-4 round-4 review-security
	// finding (sibling of the other five that I had already blocked).
	nsModes := []struct {
		name, value, reason, msg string
	}{
		{"NetworkMode", hc.NetworkMode, "network_mode_host", "HostConfig.NetworkMode=host is not permitted"},
		{"PidMode", hc.PidMode, "pid_mode_host", "HostConfig.PidMode=host is not permitted"},
		{"IpcMode", hc.IpcMode, "ipc_mode_host", "HostConfig.IpcMode=host is not permitted"},
		{"UTSMode", hc.UTSMode, "uts_mode_host", "HostConfig.UTSMode=host is not permitted"},
		{"UsernsMode", hc.UsernsMode, "userns_mode_host", "HostConfig.UsernsMode=host is not permitted"},
		{"CgroupnsMode", hc.CgroupnsMode, "cgroupns_mode_host", "HostConfig.CgroupnsMode=host is not permitted"},
	}
	for _, m := range nsModes {
		if strings.ToLower(m.value) == "host" {
			return denyDecision(http.StatusForbidden, m.reason, m.msg)
		}
	}

	// Device passthrough — any non-empty Devices is a hard reject.
	if len(hc.Devices) > 0 {
		return denyDecision(http.StatusForbidden,
			"devices_nonempty",
			"HostConfig.Devices is not permitted")
	}

	// DeviceCgroupRules: parallel cgroup-rule mechanism to Devices.
	// `"a *:* rwm"` grants unrestricted device-cgroup access; combined
	// with CAP_MKNOD (in the default capset; my CapAdd policy only
	// blocks ADDs, not what defaults grant) the agent can mknod and
	// read the host's raw disks. Cycle-4 finding #2 (CRITICAL).
	if len(hc.DeviceCgroupRules) > 0 {
		return denyDecision(http.StatusForbidden,
			"device_cgroup_rules_nonempty",
			"HostConfig.DeviceCgroupRules is not permitted (mirrors the Devices denial; the rule mechanism is equivalent")
	}

	// DeviceRequests: GPU / nvidia-container-runtime style device
	// passthrough. Cycle-4 finding #5.
	if len(hc.DeviceRequests) > 0 {
		return denyDecision(http.StatusForbidden,
			"device_requests_nonempty",
			"HostConfig.DeviceRequests is not permitted")
	}

	// VolumesFrom: inherit mounts from another container. The other
	// container's mount set is impossible to audit transitively; the
	// agent could inherit any host bind that any other container the
	// user has on the host carries. Cycle-4 finding #5.
	if len(hc.VolumesFrom) > 0 {
		return denyDecision(http.StatusForbidden,
			"volumes_from_nonempty",
			"HostConfig.VolumesFrom is not permitted (mounts cannot be audited transitively)")
	}

	// MaskedPaths / ReadonlyPaths: setting these (even as empty
	// arrays) overrides runc's safe defaults, re-exposing /proc/keys,
	// /proc/sysrq-trigger, /sys/firmware, etc. inside the container.
	// No legitimate workflow overrides these for security. Cycle-4
	// finding #3. (Pointer types distinguish field-absent from
	// field-present-with-empty-array.)
	if hc.MaskedPaths != nil {
		return denyDecision(http.StatusForbidden,
			"masked_paths_present",
			"HostConfig.MaskedPaths is not permitted (overrides runc's safe default; legitimate workflows should rely on the default)")
	}
	if hc.ReadonlyPaths != nil {
		return denyDecision(http.StatusForbidden,
			"readonly_paths_present",
			"HostConfig.ReadonlyPaths is not permitted (overrides runc's safe default)")
	}

	// Sysctls: kernel parameters set in the container. Some sysctls
	// are namespaced (safe), some are not. Defence-in-depth (cycle-5
	// opportunistic sweep): deny any Sysctls entirely; legitimate
	// workflows can request specific entries through the allowlist
	// admission process.
	if len(hc.Sysctls) > 0 {
		return denyDecision(http.StatusForbidden,
			"sysctls_nonempty",
			"HostConfig.Sysctls is not permitted (defence-in-depth; some sysctls are not namespaced)")
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
// Cycle 5: also runs with DisallowUnknownFields, so the same
// hostConfig allowlist constrains which UpdateConfig fields the
// agent may set. A previously-unknown HostConfig field cannot be
// smuggled through an update.
func (p *Proxy) inspectUpdate(body []byte) policyDecision {
	if len(bytes.TrimSpace(body)) == 0 {
		// An empty update body is a no-op upstream — forward it.
		return allowDecision("policy:containers/update:empty")
	}
	var hc hostConfig
	if dec := decodeStrict(body, &hc); !dec.allow {
		dec.reason = "update:" + dec.reason
		dec.message = "containers/update: " + dec.message
		return dec
	}
	return p.checkResourceCaps(&hc, capContextUpdate)
}

// volumeCreateBody is the explicit allowlist for POST volumes/create.
// Driver + DriverOpts are INSPECTED (cycle-4 finding #1 fix); Name
// and Labels are FORWARDED. ClusterVolumeSpec is admitted opaque for
// swarm-mode requests we are not policy-relevant for.
type volumeCreateBody struct {
	Name       string            `json:"Name"`       // INSPECTED (audit log only; no policy)
	Driver     string            `json:"Driver"`     // INSPECTED — deny local + opts
	DriverOpts map[string]string `json:"DriverOpts"` // INSPECTED with Driver

	// FORWARDED.
	Labels            json.RawMessage `json:"Labels"`
	ClusterVolumeSpec json.RawMessage `json:"ClusterVolumeSpec"`
}

// networkCreateBody is the explicit allowlist for POST
// networks/create. None of the fields are currently INSPECTED — the
// network-mode escape lives on HostConfig.NetworkMode of a container
// (already checked), not on the network definition itself. The
// inversion exists purely so a future docker-API addition cannot
// silently introduce an escape via networks/create.
type networkCreateBody struct {
	Name           json.RawMessage `json:"Name"`
	CheckDuplicate json.RawMessage `json:"CheckDuplicate"`
	Driver         json.RawMessage `json:"Driver"`
	Scope          json.RawMessage `json:"Scope"`
	EnableIPv6     json.RawMessage `json:"EnableIPv6"`
	IPAM           json.RawMessage `json:"IPAM"`
	Internal       json.RawMessage `json:"Internal"`
	Attachable     json.RawMessage `json:"Attachable"`
	Ingress        json.RawMessage `json:"Ingress"`
	ConfigFrom     json.RawMessage `json:"ConfigFrom"`
	ConfigOnly     json.RawMessage `json:"ConfigOnly"`
	Options        json.RawMessage `json:"Options"`
	Labels         json.RawMessage `json:"Labels"`
	// podman libpod additions
	ID                json.RawMessage `json:"id"`
	Created           json.RawMessage `json:"created"`
	NetworkInterface  json.RawMessage `json:"network_interface"`
	Subnets           json.RawMessage `json:"subnets"`
	IPv6Enabled       json.RawMessage `json:"ipv6_enabled"`
	DNSEnabled        json.RawMessage `json:"dns_enabled"`
	Routes            json.RawMessage `json:"routes"`
	NetworkDNSServers json.RawMessage `json:"network_dns_servers"`
}

// inspectVolumeCreate parses body as a volumes/create request and
// rejects any DriverOpts on the local driver. Cycle 5: also runs
// with DisallowUnknownFields so unknown volumes/create fields
// reject.
func (p *Proxy) inspectVolumeCreate(body []byte) policyDecision {
	if len(bytes.TrimSpace(body)) == 0 {
		return allowDecision("policy:volumes/create:empty")
	}
	var req volumeCreateBody
	if dec := decodeStrict(body, &req); !dec.allow {
		dec.reason = "volumes_create:" + dec.reason
		dec.message = "volumes/create: " + dec.message
		return dec
	}
	driver := strings.ToLower(req.Driver)
	if driver == "local" && len(req.DriverOpts) > 0 {
		return denyDecision(http.StatusForbidden,
			"volume_local_driver_opts",
			"volumes/create with Driver=local and DriverOpts is not permitted (local-driver bind-volume escape; use a containers/create Bind/Mount with an allowlisted Source instead)")
	}
	return allowDecision("policy:volumes/create:ok")
}

// inspectNetworkCreate parses body as a networks/create request and
// rejects unknown fields. No fields are currently INSPECTED — the
// network-mode escape lives on HostConfig.NetworkMode of a
// container, not on the network definition. The schema-inversion
// exists purely to lock networks/create down so a future docker-API
// field cannot silently introduce an escape via this endpoint.
func (p *Proxy) inspectNetworkCreate(body []byte) policyDecision {
	if len(bytes.TrimSpace(body)) == 0 {
		return allowDecision("policy:networks/create:empty")
	}
	var req networkCreateBody
	if dec := decodeStrict(body, &req); !dec.allow {
		dec.reason = "networks_create:" + dec.reason
		dec.message = "networks/create: " + dec.message
		return dec
	}
	return allowDecision("policy:networks/create:ok")
}

// inspectExec parses body as a containers/{id}/exec request and
// rejects Privileged: true. The exec body has its own Privileged
// field that grants additional capabilities to the exec process
// independent of the parent container's HostConfig.Privileged — the
// create-time deny does not cover it.
func (p *Proxy) inspectExec(body []byte) policyDecision {
	if len(bytes.TrimSpace(body)) == 0 {
		return allowDecision("policy:containers/exec:empty")
	}
	var req containerExecBody
	if dec := decodeStrict(body, &req); !dec.allow {
		dec.reason = "exec:" + dec.reason
		dec.message = "containers/exec: " + dec.message
		return dec
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
// after BOTH lexical cleanup (filepath.Clean, which collapses ".."
// and double slashes) AND symlink resolution (filepath.EvalSymlinks,
// which follows every symlink in the chain to its canonical target),
// the resolved source equals an allowlist entry exactly or starts
// with entry + "/". The substring trap — "/srv" allowing
// "/srv-other" — is closed by requiring the separator.
//
// The special entry "/" allows every absolute path; it exists so the
// security test suite can run a negative-control case proving the
// positive tests are not no-ops.
//
// # Symlink resolution
//
// Pre-cycle-4 (#2326 round 3), the check was purely lexical: an
// agent with write access to its worktree could create a symlink at
// (allowed-prefix)/key → /etc/passwd, bind it, and the kernel's
// mount(2) would follow the symlink and expose the host file. The
// fix is to resolve symlinks on BOTH the source and the allowlist
// entries so the prefix comparison is between canonical paths.
//
// EvalSymlinks errors on paths that do not exist, on broken symlink
// chains, and on EACCES. In every case the source is not a usable
// bind target and the proxy denies. This is intentionally stricter
// than docker, which forwards an unresolved source to runc and lets
// runc error — the proxy prefers to give the agent an actionable 4xx
// over surfacing a runc internal error, and a non-existent source on
// a bind request is suspect anyway.
//
// # Residual TOCTOU
//
// EvalSymlinks runs at policy time, mount(2) runs in podman/runc
// later. Between those two moments the agent COULD swap a resolved-
// safe path for a symlink pointing somewhere dangerous. The window
// is small and the bind point inside the container is fixed at
// create time, but the residual risk is real. Acceptable for v1 on
// the basis that:
//
//   - Closing the TOCTOU window requires either kernel-level fs
//     freezing primitives (not portable) or filesystem snapshots
//     (heavyweight, out of scope for a Step-1 library PR).
//   - The agent process is otherwise sandboxed; arming the race in
//     the first place still requires write access to a path inside
//     an allowed prefix, which the worktree-only bind allowlist
//     already restricts.
//   - Defence-in-depth at the sandbox layer (bwrap / sandbox-exec)
//     remains in front — the proxy is one link in a chain, not the
//     sole boundary.
//
// Step 3 and onwards should consider whether the per-session
// scratch dir (which is the agent's only realistic vector for
// creating a malicious symlink) should be mounted with `nosymfollow`
// or similar where the kernel/platform supports it. Out of scope
// for this PR but worth a comment in #2317 when Step 3 lands.
func (p *Proxy) isAllowedBindSource(src string) bool {
	if src == "" {
		return false
	}
	if !strings.HasPrefix(src, "/") {
		// Relative paths are never allowed — they cannot be validated
		// against an absolute-path allowlist, and docker's Binds spec
		// requires absolute sources anyway. Defence-in-depth: reject
		// rather than relying on docker to reject downstream.
		return false
	}
	canonicalSrc, ok := canonicalisePath(src)
	if !ok {
		return false
	}
	for _, raw := range p.cfg.AllowedBindSources {
		if raw == "" {
			continue
		}
		// Resolve the allowlist entry too — if the host's TMPDIR (or
		// any other path in the allowlist) goes through a symlink
		// like /tmp→/private/tmp on macOS, the source's canonical
		// form will only prefix-match the allowlist's canonical form.
		// Fall back to lexical when the entry does not currently
		// exist so a yet-to-be-created scratch dir does not silently
		// deny-all.
		allowed := filepath.Clean(raw)
		if resolved, ok := canonicalisePath(raw); ok {
			allowed = resolved
		}
		if allowed == "/" {
			return true
		}
		if canonicalSrc == allowed {
			return true
		}
		if strings.HasPrefix(canonicalSrc, allowed+"/") {
			return true
		}
	}
	return false
}

// canonicalisePath returns the canonical (lexically cleaned + symlink-
// resolved) form of p, or ("", false) if p is not a usable path on
// the current host — does not exist, is a broken symlink chain, or
// is otherwise unreachable. Defensive about every error mode of
// filepath.EvalSymlinks because the call sites depend on a strict
// canonical form for the prefix-match security check.
func canonicalisePath(p string) (string, bool) {
	if p == "" {
		return "", false
	}
	cleaned := filepath.Clean(p)
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err != nil {
		return "", false
	}
	return resolved, true
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
