package podmanproxy

// Per-session image ledger.
//
// A container image pulled through the proxy lands in the HOST image
// store, which the user and every other session share. Nothing in the
// session's own teardown removes it, so a session that pulls a large
// toolchain image leaks that storage forever. The ledger closes the
// gap: the proxy appends one line per admitted POST /images/create,
// and `prism cleanup` replays the ledger and removes each reference.
//
// The ledger is deliberately NOT a host-wide prune. Other sessions and
// the user share the image store, so the sweep may only name the
// references this session asked for.
//
// Two halves live here:
//
//   - The WRITE half (recordPulledImage, called from the handler) runs
//     inside the proxy process.
//   - The READ half (ReadImageLedger, called from cmd/cleanup_sweep.go)
//     runs inside `prism cleanup` after the session's sidecar is gone.
//
// Both halves apply imageRefIsSweepable to the reference. Validating on
// write keeps garbage out of the file; validating again on read means a
// hand-edited or truncated ledger cannot turn into a podman argument
// that the sweep did not intend. The check is the load-bearing defence
// against argument injection: a reference is agent-controlled, and
// `podman rmi --force` (or any other flag-shaped string) must never
// reach the command line.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
)

// ImageLedgerFileName is the basename of the per-session image ledger,
// written next to the audit log under the per-session work dir
// (<XDG_STATE_HOME>/prism/sessions/<instanceID>/). The sidecar opens it
// for append; cmd/cleanup_sweep.go reads it before the work dir is
// removed.
const ImageLedgerFileName = "podman-images.log"

// maxImageLedgerBytes caps how much of a ledger file ReadImageLedger
// will consume. A ledger line is at most a few hundred bytes and a
// session pulls a handful of images, so 1 MiB is several orders of
// magnitude of headroom. The cap exists so a corrupted or adversarially
// grown file cannot make cleanup allocate without bound.
const maxImageLedgerBytes = 1 << 20

// maxImageRefLen bounds a single image reference. Registry references
// are host/namespace/name:tag or host/namespace/name@sha256:<64 hex>;
// 256 bytes covers every realistic shape with room to spare.
const maxImageRefLen = 256

// imageRefPattern is the sweepable-reference character allowlist. It is
// deliberately narrower than what a registry accepts:
//
//   - The first character MUST be alphanumeric. This is what makes a
//     reference unusable as a command-line flag: no "-f", no
//     "--force", no leading "/" pointing somewhere unexpected.
//   - The remaining characters are the registry-reference character
//     set: letters, digits, and the separators . _ - : / @ that make
//     up host:port/namespace/name:tag and the @sha256:<digest> form.
//
// Anything else is dropped rather than sanitised. A reference the
// proxy cannot represent exactly is a reference the sweep must not
// guess at.
//
// The character set alone is NOT sufficient — see imageRefIsSweepable
// for the structural check that has to run with it.
var imageRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]*$`)

// imageLedgerLine is the JSON shape of one ledger entry. JSON (rather
// than a bare newline-delimited string) is the format because the
// reference is agent-controlled: a query parameter can carry a literal
// newline via %0A, and a plain-text ledger would let the agent forge
// extra entries. encoding/json escapes the separator, so one request
// can only ever produce one line.
type imageLedgerLine struct {
	Image string `json:"image"`
}

// imageRefIsSweepable reports whether ref is safe to write to the
// ledger, to pass to `podman rmi` as a positional argument, AND to
// embed in the path of the upstream existence probe.
//
// Two checks, and both are load-bearing:
//
//  1. The character allowlist (imageRefPattern). This is what stops a
//     reference from looking like a command-line flag.
//  2. A path-structure check. The character set admits `.` and `/`, so
//     it admits dot segments: "a/../../libpod/secrets/x" passes check 1
//     unchanged. imageExistsUpstream embeds the reference in a URL path
//     on the privileged podman socket, Go transmits dot segments
//     verbatim, and podman's router cleans the path and redirects — so
//     a reference like that turns the probe into a GET against an
//     endpoint the proxy's own allowlist denies, and the status code
//     leaks back as an oracle. Requiring the reference to equal its own
//     path.Clean, with no leading or trailing slash, removes every dot
//     segment and every empty segment.
//
// A registry reference never legitimately contains a dot SEGMENT.
// "alpine:3.19", "docker.io/library/alpine", and
// "registry:5000/ns/img@sha256:abc" all satisfy path.Clean(ref) == ref.
func imageRefIsSweepable(ref string) bool {
	if ref == "" || len(ref) > maxImageRefLen {
		return false
	}
	if !imageRefPattern.MatchString(ref) {
		return false
	}
	// Structural check. path.Clean collapses ".", "..", and "//", so any
	// reference carrying one of them differs from its cleaned form.
	// A trailing slash is also removed by Clean, so it is caught here
	// too; a leading slash cannot occur because check 1 requires an
	// alphanumeric first character.
	return path.Clean(ref) == ref
}

// pulledImageRef derives the image reference a POST /images/create
// request asks the upstream to fetch, or "" when the request names no
// reference the sweep could remove later.
//
// The docker/podman API spells the reference across three query
// parameters:
//
//   - fromImage — the pull surface. "alpine", "docker.io/library/
//     alpine", "alpine:3.19", "alpine@sha256:<digest>".
//   - repo — the import surface (paired with fromSrc). Names the
//     repository the imported layer is filed under.
//   - tag — the tag OR digest, supplied separately from the name.
//
// The two name parameters are mutually exclusive in practice;
// fromImage wins when both are present, matching podman's own
// precedence. tag is appended only when the name does not already
// carry its own tag or digest, because docker clients send both
// forms and appending unconditionally would produce
// "alpine:3.19:latest".
//
// A request with neither name parameter (a bare fromSrc import) is
// unrecordable: the resulting image has no reference the sweep could
// name. Those are left for the user, exactly as an unprefixed
// user-named container is.
func pulledImageRef(query url.Values) string {
	name := strings.TrimSpace(query.Get("fromImage"))
	if name == "" {
		name = strings.TrimSpace(query.Get("repo"))
	}
	if name == "" {
		return ""
	}

	if tag := strings.TrimSpace(query.Get("tag")); tag != "" && !refCarriesTag(name) {
		// A tag parameter holding a digest ("sha256:<hex>") joins with
		// '@', not ':'. This is the same rule the docker CLI applies
		// when it splits a reference before sending it.
		if strings.Contains(tag, ":") {
			name += "@" + tag
		} else {
			name += ":" + tag
		}
	}

	if !imageRefIsSweepable(name) {
		return ""
	}
	return name
}

// refCarriesTag reports whether ref already ends in a tag or a digest.
//
// The ':' that separates a tag is ambiguous with the ':' in a registry
// host:port, so the check applies to the LAST path segment only:
// "registry:5000/alpine" carries no tag, "registry:5000/alpine:3.19"
// does.
func refCarriesTag(ref string) bool {
	last := ref
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		last = ref[i+1:]
	}
	return strings.ContainsAny(last, ":@")
}

// recordPulledImage writes one ledger line for an admitted
// POST /images/create and returns the audit reason for the request.
//
// The reason names the outcome so the audit log answers "why was this
// image not removed at cleanup?" without a second source:
//
//   - policy:images/create:recorded:<ref> — the reference is in the
//     ledger and the sweep will try to remove it.
//   - policy:images/create:already_present — the image was in the
//     upstream store BEFORE this request. Not recorded. See below.
//   - policy:images/create:probe_failed — the proxy could not
//     establish whether the image was already present. Not recorded.
//   - policy:images/create:unrecordable — the request named no
//     sweepable reference (bare fromSrc import, or a reference outside
//     imageRefPattern).
//
// In every case the pull itself forwards. The ledger never gates a
// request; it only decides what cleanup removes later.
//
// # Why the already-present check exists
//
// The ledger drives `podman rmi` at cleanup, against a store the user
// and every other session share. Recording the requested reference
// alone would hand the agent a deferred deletion primitive: a pull of
// an image the host already has would enrol that image for removal,
// and prism — not the agent — would delete it after the session ended.
// The issue scopes the sweep to "the images that the session pulled",
// so an image that was already there is out of scope by definition.
//
// The probe is one read-only GET against the upstream. It is best
// effort and it fails CLOSED: anything other than a confident "not
// present" leaves the image unrecorded. An unrecorded image leaks
// storage, which is the failure this whole feature exists to reduce.
// A wrongly recorded one destroys someone else's data. The two are not
// symmetric, so the tie goes to leaking.
//
// A concurrent pull of the same image by two sessions can record it
// twice. That is acceptable: `podman rmi` on an image still in use
// fails, and the sweep already ignores that failure.
//
// A write failure is swallowed on purpose. The ledger is a
// housekeeping aid: losing a line leaks storage, while failing the
// request would break a legitimate pull over a full disk.
func (p *Proxy) recordPulledImage(ctx context.Context, query url.Values) string {
	ref := pulledImageRef(query)
	if ref == "" {
		return "policy:images/create:unrecordable"
	}
	if p.cfg.PulledImageWriter == nil {
		return "policy:images/create:recorded:" + truncateForReason(ref)
	}

	switch present, err := p.imageExistsUpstream(ctx, ref); {
	case err != nil:
		return "policy:images/create:probe_failed:" + truncateForReason(ref)
	case present:
		return "policy:images/create:already_present:" + truncateForReason(ref)
	}

	encoded, err := json.Marshal(imageLedgerLine{Image: ref})
	if err != nil {
		// imageLedgerLine holds one string field, so Marshal cannot
		// fail in practice. Drop the line rather than write garbage.
		return "policy:images/create:unrecordable"
	}
	encoded = append(encoded, '\n')

	p.imageMu.Lock()
	defer p.imageMu.Unlock()
	_, _ = p.cfg.PulledImageWriter.Write(encoded)
	return "policy:images/create:recorded:" + truncateForReason(ref)
}

// imageExistsUpstream reports whether ref already resolves to an image
// in the upstream store, using the docker-compat inspect endpoint
// (GET /images/{name}/json).
//
// The three outcomes are distinct and the caller treats them
// differently:
//
//   - (false, nil) — the upstream answered 404. The image is not
//     there, so this request is a genuine new pull.
//   - (true, nil)  — the upstream answered 2xx. The image is already
//     there.
//   - (_, err)     — anything else: a transport failure, or a status
//     the proxy cannot interpret. The caller must not record.
//
// ref is embedded in the path unescaped on purpose. A registry
// reference carries '/' and ':' as structural separators and the
// docker route matches the remainder of the path, so percent-encoding
// them breaks the lookup.
//
// That is only safe because imageRefIsSweepable has already run. It
// rejects a reference carrying a space, '?', or '#' — which would
// otherwise split the path — AND rejects one carrying a dot segment,
// which would otherwise redirect this probe onto an endpoint the proxy
// allowlist denies. Do not call this function with a reference that has
// not passed that check.
//
// Redirects are not followed, as defence in depth behind that check: a
// probe must resolve on the route it names or not at all.
func (p *Proxy) imageExistsUpstream(ctx context.Context, ref string) (bool, error) {
	if p.probeClient == nil {
		return false, fmt.Errorf("podmanproxy: no upstream probe client")
	}
	if !imageRefIsSweepable(ref) {
		return false, fmt.Errorf("podmanproxy: image probe: reference is not probe-safe")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://podman.sock/images/"+ref+"/json", nil)
	if err != nil {
		return false, fmt.Errorf("podmanproxy: build image probe: %w", err)
	}
	resp, err := p.probeClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("podmanproxy: image probe: %w", err)
	}
	defer resp.Body.Close()
	// The body is not needed, but it must be drained for the connection
	// to return to the idle pool.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return true, nil
	default:
		return false, fmt.Errorf("podmanproxy: image probe: unexpected status %d", resp.StatusCode)
	}
}

// ReadImageLedger reads the per-session image ledger at path and
// returns the distinct image references it names, in first-seen order.
//
// Contract for the caller (`prism cleanup`):
//
//   - A missing file returns (nil, nil). A session that never pulled an
//     image, and a session that never enabled containers at all, both
//     take this path — so the caller issues no podman command for them.
//   - A malformed line is skipped, not fatal. The ledger is appended to
//     by a process that can be killed mid-write, so a torn last line is
//     an expected state, not a corruption to report.
//   - A reference outside imageRefPattern is skipped. See the file
//     comment: this is the argument-injection defence, and it is
//     applied on read as well as on write.
//
// Only the first maxImageLedgerBytes of the file are consumed.
func ReadImageLedger(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("podmanproxy: open image ledger %q: %w", path, err)
	}
	defer f.Close()

	var (
		refs []string
		seen = map[string]struct{}{}
	)
	sc := bufio.NewScanner(io.LimitReader(f, maxImageLedgerBytes))
	sc.Buffer(make([]byte, 0, 4096), maxImageRefLen+256)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var entry imageLedgerLine
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if !imageRefIsSweepable(entry.Image) {
			continue
		}
		if _, dup := seen[entry.Image]; dup {
			continue
		}
		seen[entry.Image] = struct{}{}
		refs = append(refs, entry.Image)
	}
	// A scanner error (including bufio.ErrTooLong on an over-long line)
	// ends the read early; whatever was parsed before it still stands.
	// Returning it would make cleanup report a failure for a file it
	// successfully drained the useful part of.
	return refs, nil
}
