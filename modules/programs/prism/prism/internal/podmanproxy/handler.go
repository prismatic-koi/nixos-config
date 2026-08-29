package podmanproxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
)

// serveHTTP is the proxy's single HTTP handler. The control flow is:
//
//  1. Normalise the request path; reject anything that does not look
//     like a docker / podman REST path.
//  2. Classify the (method, path) pair against the endpoint allowlist.
//  3. For endpoints that need body inspection, read the body into
//     memory subject to MaxBodyBytes, parse the policy fields, and
//     either reject (synthesised 4xx) or restore the body and forward.
//  4. For endpoints that need query inspection (PUT archive), check
//     the `path` query, then forward.
//  5. For streaming endpoints, forward without touching the body.
//  6. For plain allow endpoints, forward as-is.
//  7. For deny endpoints, synthesise a 403 with a friendly envelope
//     that names the rejected (method, path).
//
// Every branch emits exactly one audit line before returning.
func (p *Proxy) serveHTTP(w http.ResponseWriter, r *http.Request) {
	normPath := normalisePath(r.URL.Path)
	kind := classifyRequest(r.Method, normPath)

	switch kind {
	case endpointAllow:
		p.emitAudit(r, auditAllow, "policy:endpoint_allow:"+normPath)
		p.upstream.ServeHTTP(w, r)

	case endpointAllowStreaming:
		p.emitAudit(r, auditAllow, "policy:endpoint_streaming:"+normPath)
		p.upstream.ServeHTTP(w, r)

	case endpointPolicyCreate:
		p.handlePolicyCreate(w, r)

	case endpointPolicyArchive:
		p.handlePolicyArchive(w, r)

	case endpointPolicyUpdate:
		p.handlePolicyUpdate(w, r)

	case endpointPolicyExec:
		p.handlePolicyExec(w, r)

	case endpointPolicyVolumeCreate:
		p.handlePolicyVolumeCreate(w, r)

	case endpointPolicyNetworkCreate:
		p.handlePolicyNetworkCreate(w, r)

	case endpointPolicyRename:
		p.handlePolicyRename(w, r)

	case endpointDeny:
		fallthrough
	default:
		reason := "endpoint_not_allowed"
		msg := fmt.Sprintf("endpoint %s %s is not permitted by the prism podman proxy", r.Method, r.URL.Path)
		p.emitAudit(r, auditDeny, reason+":"+r.Method+" "+truncateForReason(normPath))
		writeJSONError(w, http.StatusForbidden, msg)
	}
}

// handlePolicyCreate is the body-inspecting branch for POST
// /containers/create. The body is read in full into memory (bounded by
// MaxBodyBytes), parsed, evaluated, and — if allowed — restored on the
// request before forwarding upstream.
//
// The body restore step is the subtle bit: httputil.ReverseProxy reads
// r.Body when copying the request to the upstream Transport. After
// io.ReadAll the original ReadCloser is drained, so we substitute an
// io.NopCloser-wrapped bytes.Reader and set ContentLength explicitly so
// the upstream sees a valid Content-Length header.
func (p *Proxy) handlePolicyCreate(w http.ResponseWriter, r *http.Request) {
	body, readDec := p.readBoundedBody(w, r)
	if !readDec.allow {
		p.emitAudit(r, auditDeny, readDec.reason)
		writeJSONError(w, readDec.status, readDec.message)
		return
	}

	res := p.inspectCreate(body, r.URL.Query())
	if !res.decision.allow {
		p.emitAudit(r, auditDeny, res.decision.reason)
		writeJSONError(w, res.decision.status, res.decision.message)
		return
	}

	// inspectCreate may have rewritten the body and/or the URL query
	// when the Name-prefix policy injected an auto-prefixed Name. The
	// injection branch writes the same name into BOTH channels so the
	// upstream sees a consistent name no matter which channel (libpod
	// body Name vs docker-compat ?name=) its handler reads. When either
	// rewritten field is empty the original is forwarded unchanged.
	forwardBody := body
	if res.rewrittenBody != nil {
		forwardBody = res.rewrittenBody
	}
	if res.rewrittenQuery != "" {
		r.URL.RawQuery = res.rewrittenQuery
	}
	r.Body = io.NopCloser(bytes.NewReader(forwardBody))
	r.ContentLength = int64(len(forwardBody))

	p.emitAudit(r, auditAllow, res.decision.reason)
	p.upstream.ServeHTTP(w, r)
}

// handlePolicyRename is the query-inspecting branch for POST
// /containers/{id}/rename. The body is empty for this endpoint, so
// the policy check operates purely on the URL query and forwards
// the request unmodified on allow.
func (p *Proxy) handlePolicyRename(w http.ResponseWriter, r *http.Request) {
	dec := p.inspectRename(r.URL.Query())
	if !dec.allow {
		p.emitAudit(r, auditDeny, dec.reason)
		writeJSONError(w, dec.status, dec.message)
		return
	}
	p.emitAudit(r, auditAllow, dec.reason)
	p.upstream.ServeHTTP(w, r)
}

// handlePolicyArchive is the query-inspecting branch for PUT
// /containers/{id}/archive. The body is a tar stream and is forwarded
// without being touched.
func (p *Proxy) handlePolicyArchive(w http.ResponseWriter, r *http.Request) {
	dec := p.inspectArchive(r)
	if !dec.allow {
		p.emitAudit(r, auditDeny, dec.reason)
		writeJSONError(w, dec.status, dec.message)
		return
	}
	p.emitAudit(r, auditAllow, dec.reason)
	p.upstream.ServeHTTP(w, r)
}

// handlePolicyUpdate is the body-inspecting branch for POST
// /containers/{id}/update. Mirrors handlePolicyCreate in shape: read
// body bounded by MaxBodyBytes, parse JSON, apply resource-cap
// policy in update-context mode, then either reject or restore the
// body and forward.
func (p *Proxy) handlePolicyUpdate(w http.ResponseWriter, r *http.Request) {
	body, readDec := p.readBoundedBody(w, r)
	if !readDec.allow {
		p.emitAudit(r, auditDeny, readDec.reason)
		writeJSONError(w, readDec.status, readDec.message)
		return
	}
	dec := p.inspectUpdate(body)
	if !dec.allow {
		p.emitAudit(r, auditDeny, dec.reason)
		writeJSONError(w, dec.status, dec.message)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	p.emitAudit(r, auditAllow, dec.reason)
	p.upstream.ServeHTTP(w, r)
}

// handlePolicyNetworkCreate is the body-inspecting branch for POST
// /networks/create. Same shape as the other policy handlers; the
// inspection itself just runs the schema-inversion pass.
func (p *Proxy) handlePolicyNetworkCreate(w http.ResponseWriter, r *http.Request) {
	body, readDec := p.readBoundedBody(w, r)
	if !readDec.allow {
		p.emitAudit(r, auditDeny, readDec.reason)
		writeJSONError(w, readDec.status, readDec.message)
		return
	}
	dec := p.inspectNetworkCreate(body)
	if !dec.allow {
		p.emitAudit(r, auditDeny, dec.reason)
		writeJSONError(w, dec.status, dec.message)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	p.emitAudit(r, auditAllow, dec.reason)
	p.upstream.ServeHTTP(w, r)
}

// handlePolicyVolumeCreate is the body-inspecting branch for POST
// /volumes/create. Same shape as handlePolicyUpdate: read body,
// inspect for the local-driver bind-volume escape, restore body and
// forward on allow.
func (p *Proxy) handlePolicyVolumeCreate(w http.ResponseWriter, r *http.Request) {
	body, readDec := p.readBoundedBody(w, r)
	if !readDec.allow {
		p.emitAudit(r, auditDeny, readDec.reason)
		writeJSONError(w, readDec.status, readDec.message)
		return
	}
	dec := p.inspectVolumeCreate(body)
	if !dec.allow {
		p.emitAudit(r, auditDeny, dec.reason)
		writeJSONError(w, dec.status, dec.message)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	p.emitAudit(r, auditAllow, dec.reason)
	p.upstream.ServeHTTP(w, r)
}

// handlePolicyExec is the body-inspecting branch for POST
// /containers/{id}/exec. Same shape as handlePolicyUpdate, but the
// policy is the exec-Privileged check rather than resource caps.
func (p *Proxy) handlePolicyExec(w http.ResponseWriter, r *http.Request) {
	body, readDec := p.readBoundedBody(w, r)
	if !readDec.allow {
		p.emitAudit(r, auditDeny, readDec.reason)
		writeJSONError(w, readDec.status, readDec.message)
		return
	}
	dec := p.inspectExec(body)
	if !dec.allow {
		p.emitAudit(r, auditDeny, dec.reason)
		writeJSONError(w, dec.status, dec.message)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	p.emitAudit(r, auditAllow, dec.reason)
	p.upstream.ServeHTTP(w, r)
}
