package prismcontainer

import (
	"fmt"
	"path/filepath"
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
	DefaultTimeout      = 10 * time.Minute
	DefaultBuildTimeout = 30 * time.Minute
	MaxTimeout          = 60 * time.Minute
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

	timeout, err := validTimeout(r.TimeoutSeconds, DefaultTimeout)
	if err != nil {
		return validRun{}, err
	}

	v := validRun{image: image, command: r.Command, mount: mount, timeout: timeout}
	for _, e := range r.Env {
		key, err := validKeyValue("--env", e)
		if err != nil {
			return validRun{}, err
		}
		v.env = append(v.env, e)
		v.envKeys = append(v.envKeys, key)
	}
	return v, nil
}

// validTimeout returns the request timeout. Zero selects def.
func validTimeout(seconds int64, def time.Duration) (time.Duration, error) {
	timeout := def
	if seconds != 0 {
		timeout = time.Duration(seconds) * time.Second
	}
	if timeout <= 0 {
		return 0, fmt.Errorf("--timeout must be more than zero")
	}
	if timeout > MaxTimeout {
		return 0, fmt.Errorf("--timeout %s is more than the maximum of %s", timeout, MaxTimeout)
	}
	return timeout, nil
}

// validKeyValue checks one KEY=VALUE input and returns the key. An entry
// with no "=" makes podman copy the value from its own environment, which
// is the host environment of prism, so it is refused.
func validKeyValue(flag, e string) (string, error) {
	key, _, ok := strings.Cut(e, "=")
	if !ok {
		return "", fmt.Errorf("%s %q has no \"=\": use KEY=VALUE", flag, e)
	}
	if !envKeyPattern.MatchString(key) {
		return "", fmt.Errorf("%s key %q is not valid: it must match %s", flag, key, envKeyPattern.String())
	}
	return key, nil
}

// BuildRequest holds every input the agent controls for `prism container
// build`. It is also the wire schema of the host-API endpoint POST
// /container/build. Prism sets every other podman option.
type BuildRequest struct {
	// Context is the build context directory, relative to the worktree.
	// Empty means the worktree root.
	Context string `json:"context,omitempty"`
	// File is the Containerfile, relative to the worktree. Empty means
	// Containerfile, then Dockerfile, in the context directory.
	File           string   `json:"file,omitempty"`
	BuildArgs      []string `json:"build_args,omitempty"`
	Tag            string   `json:"tag,omitempty"`
	TimeoutSeconds int64    `json:"timeout_seconds,omitempty"`
}

// validBuild is a BuildRequest after validation. The paths are clean and
// relative, and are not yet resolved against the worktree.
type validBuild struct {
	context      string
	file         string
	buildArgs    []string
	buildArgKeys []string
	tag          string
	timeout      time.Duration
}

// maxBuildNameLength limits the name part of --tag, so that the image
// name with the session prefix stays under the reference length limit.
const maxBuildNameLength = 64

var buildTagPattern = regexp.MustCompile(`^` + refPathComponent + `(?::` + refTag + `)?$`)

func (r BuildRequest) validate() (validBuild, error) {
	ctx, err := worktreeRelPath("CONTEXT", r.Context)
	if err != nil {
		return validBuild{}, err
	}
	v := validBuild{context: ctx}
	if r.File != "" {
		if v.file, err = worktreeRelPath("--file", r.File); err != nil {
			return validBuild{}, err
		}
	}
	if r.Tag != "" {
		name, _, _ := strings.Cut(r.Tag, ":")
		if !buildTagPattern.MatchString(r.Tag) || len(name) > maxBuildNameLength {
			return validBuild{}, fmt.Errorf("--tag %q is not a valid image tag: use NAME or NAME:TAG, where NAME has at most %d lower-case letters, digits, and separators (. _ -), and TAG has at most 128 letters, digits, and . _ -",
				r.Tag, maxBuildNameLength)
		}
		v.tag = r.Tag
	}
	if v.timeout, err = validTimeout(r.TimeoutSeconds, DefaultBuildTimeout); err != nil {
		return validBuild{}, err
	}
	for _, a := range r.BuildArgs {
		key, err := validKeyValue("--build-arg", a)
		if err != nil {
			return validBuild{}, err
		}
		v.buildArgs = append(v.buildArgs, a)
		v.buildArgKeys = append(v.buildArgKeys, key)
	}
	return v, nil
}

// worktreeRelPath cleans a path that the agent gives relative to the
// worktree. It refuses a path that is absolute or that leaves the worktree
// by "..". A symlink can still lead out. resolveInWorktree checks that.
func worktreeRelPath(flag, p string) (string, error) {
	if p == "" {
		return ".", nil
	}
	if strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("%s %q holds a NUL byte", flag, p)
	}
	if filepath.IsAbs(p) {
		return "", fmt.Errorf("%s %q is an absolute path: give a path relative to the worktree", flag, p)
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s %q is outside the worktree", flag, p)
	}
	return clean, nil
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
