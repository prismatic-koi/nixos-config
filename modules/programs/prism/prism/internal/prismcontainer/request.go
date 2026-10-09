package prismcontainer

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Mount selects how the session worktree appears at /workspace.
type Mount string

const (
	MountNone Mount = "none"
	MountRO   Mount = "ro"
	MountRW   Mount = "rw"
)

// MountModes lists the valid --mount values. The CLI validation and the
// agent-context enum both read it.
var MountModes = []string{string(MountNone), string(MountRO), string(MountRW)}

const (
	DefaultTimeout = 10 * time.Minute
	MaxTimeout     = 60 * time.Minute
)

// RunRequest holds every input the agent controls for `prism container run`.
// It is also the wire schema of the host-API endpoint POST /container/run.
// Prism sets every other podman option.
type RunRequest struct {
	Image          string   `json:"image"`
	Command        []string `json:"command,omitempty"`
	Env            []string `json:"env,omitempty"`
	Mount          Mount    `json:"mount,omitempty"`
	TimeoutSeconds int64    `json:"timeout_seconds,omitempty"`
}

// validRun is a RunRequest after validation and normalisation.
type validRun struct {
	image   string
	command []string
	env     []string
	envKeys []string
	mount   Mount
	timeout time.Duration
}

var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (r RunRequest) validate() (validRun, error) {
	image, err := normaliseImage(r.Image)
	if err != nil {
		return validRun{}, err
	}

	mount := r.Mount
	if mount == "" {
		mount = MountRO
	}
	if mount != MountNone && mount != MountRO && mount != MountRW {
		return validRun{}, fmt.Errorf("--mount %q is not valid: use one of %s", mount, strings.Join(MountModes, ", "))
	}

	timeout := DefaultTimeout
	if r.TimeoutSeconds != 0 {
		timeout = time.Duration(r.TimeoutSeconds) * time.Second
	}
	if timeout <= 0 {
		return validRun{}, fmt.Errorf("--timeout must be more than zero")
	}
	if timeout > MaxTimeout {
		return validRun{}, fmt.Errorf("--timeout %s is more than the maximum of %s", timeout, MaxTimeout)
	}

	v := validRun{image: image, command: r.Command, mount: mount, timeout: timeout}
	for _, e := range r.Env {
		// An entry with no "=" makes podman copy the value from its own
		// environment, which is the host environment of prism. Refuse it.
		key, _, ok := strings.Cut(e, "=")
		if !ok {
			return validRun{}, fmt.Errorf("--env %q has no \"=\": use KEY=VALUE", e)
		}
		if !envKeyPattern.MatchString(key) {
			return validRun{}, fmt.Errorf("--env key %q is not valid: it must match %s", key, envKeyPattern.String())
		}
		v.env = append(v.env, e)
		v.envKeys = append(v.envKeys, key)
	}
	return v, nil
}

// Image references use the docker distribution reference grammar.
const (
	refAlnum           = `[a-z0-9]+`
	refSeparator       = `(?:[._]|__|[-]+)`
	refPathComponent   = refAlnum + `(?:` + refSeparator + refAlnum + `)*`
	refDomainComponent = `(?:[a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9-]*[a-zA-Z0-9])`
	refDomain          = `(?:` + refDomainComponent + `(?:\.` + refDomainComponent + `)*|\[[a-fA-F0-9:]+\])(?::[0-9]+)?`
	refTag             = `[\w][\w.-]{0,127}`
	refDigest          = `[A-Za-z][A-Za-z0-9]*(?:[-_+.][A-Za-z][A-Za-z0-9]*)*:[0-9a-fA-F]{32,}`
	maxImageRefLength  = 512
)

var (
	imageRefPattern = regexp.MustCompile(`^(?:` + refDomain + `/)?` + refPathComponent + `(?:/` + refPathComponent + `)*` +
		`(?::` + refTag + `)?(?:@` + refDigest + `)?$`)
	imageIDPattern = regexp.MustCompile(`^[a-f0-9]{12,64}$`)
)

// imageTransports are the containers/image transport names. Podman reads a
// reference that starts with "<transport>:" as that transport, and several
// transports read a path on the host. Prism refuses them.
var imageTransports = map[string]bool{
	"containers-storage": true,
	"dir":                true,
	"docker":             true,
	"docker-archive":     true,
	"docker-daemon":      true,
	"oci":                true,
	"oci-archive":        true,
	"ostree":             true,
	"sif":                true,
	"tarball":            true,
}

// normaliseImage validates an image reference and returns the form that
// prism gives to podman. A reference with no registry gets the Docker Hub
// prefix, as docker does it ("alpine" → "docker.io/library/alpine"), so the
// result does not depend on the registries.conf of the host. An image ID
// stays as it is.
func normaliseImage(ref string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("the image reference is empty")
	}
	// Podman parses a leading "-" as an option.
	if strings.HasPrefix(ref, "-") {
		return "", fmt.Errorf("image reference %q starts with \"-\"", ref)
	}
	if len(ref) > maxImageRefLength {
		return "", fmt.Errorf("image reference is longer than %d bytes", maxImageRefLength)
	}
	if imageIDPattern.MatchString(ref) {
		return ref, nil
	}
	if !imageRefPattern.MatchString(ref) {
		return "", fmt.Errorf("image reference %q is not a valid registry reference", ref)
	}

	first, rest, hasSlash := strings.Cut(ref, "/")
	if hasSlash && (strings.ContainsAny(first, ".:") || first == "localhost") {
		host, _, _ := strings.Cut(first, ":")
		if imageTransports[host] {
			return "", fmt.Errorf("image reference %q names the %q transport; prism accepts registry references only", ref, host)
		}
		return ref, nil
	}
	if !hasSlash {
		return "docker.io/library/" + ref, nil
	}
	return "docker.io/" + first + "/" + rest, nil
}
