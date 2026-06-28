package podmanproxy

import (
	"encoding/json"
	"net/http"
	"time"
)

// auditLine is the per-request structured log entry written to
// Config.AuditWriter. Field order in the JSON output is fixed by
// encoding/json's struct-field iteration order, which gives a stable
// shape for downstream log consumers.
//
// AC reference (#2318): "Every accepted and rejected request emits
// exactly one line to the configured audit io.Writer with fields
// timestamp, method, endpoint, decision, reason as JSON."
type auditLine struct {
	Timestamp string `json:"timestamp"`
	Method    string `json:"method"`
	Endpoint  string `json:"endpoint"`
	Decision  string `json:"decision"`
	Reason    string `json:"reason"`
}

// auditDecision* constants name the values the `decision` audit field
// may take. There are exactly two: every request is either an allow or
// a deny — upstream availability is NOT a separate decision; an allowed
// request whose upstream is unreachable still audits as "allow".
const (
	auditAllow = "allow"
	auditDeny  = "deny"
)

// emitAudit writes a single JSON line to Config.AuditWriter describing
// the policy outcome for r. It is safe for concurrent use by multiple
// goroutines: writes are serialised by p.auditMu so two simultaneous
// audit lines never interleave on the wire.
//
// emitAudit is called exactly once per request. The placement of those
// calls in serveHTTP is the source of truth for the "exactly one line
// per request" AC — do not add additional call sites from inside the
// upstream-error handler or any downstream policy helper.
func (p *Proxy) emitAudit(r *http.Request, decision, reason string) {
	if p.cfg.AuditWriter == nil {
		return
	}

	clock := p.cfg.Clock
	if clock == nil {
		clock = time.Now
	}

	line := auditLine{
		Timestamp: clock().UTC().Format(time.RFC3339Nano),
		Method:    r.Method,
		Endpoint:  r.URL.RequestURI(),
		Decision:  decision,
		Reason:    reason,
	}

	encoded, err := json.Marshal(line)
	if err != nil {
		// auditLine has only string fields, so json.Marshal cannot
		// fail in practice; on the impossible path, drop the line
		// rather than poisoning the log with garbage.
		return
	}
	encoded = append(encoded, '\n')

	p.auditMu.Lock()
	defer p.auditMu.Unlock()
	_, _ = p.cfg.AuditWriter.Write(encoded)
}
