package sidecar

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/prismatic-koi/prism/internal/hostapiauth"
)

// hostAPIRefusedMessage is the error of a host-API request that does not
// carry the secret of this sidecar. It does not say which session the
// sidecar serves.
const hostAPIRefusedMessage = "unauthorized: the request does not carry the host-API credential of this session"

// writeHostAPISecret writes a new secret next to the host-API socket and
// keeps it in memory. Run calls it before it binds the socket, so the
// listener never serves a request while the sidecar has no secret.
func (s *Sidecar) writeHostAPISecret() error {
	secret, err := hostapiauth.WriteNewSecret(hostapiauth.SecretPath(s.cfg.HostAPISockPath))
	if err != nil {
		return fmt.Errorf("sidecar: host-API credential: %w", err)
	}
	s.hostAPISecret.Store(&secret)
	return nil
}

// currentHostAPISecret returns the secret of this sidecar, or "" before Run
// wrote it.
func (s *Sidecar) currentHostAPISecret() string {
	if p := s.hostAPISecret.Load(); p != nil {
		return *p
	}
	return ""
}

// requireHostAPICredential wraps the host-API routes. A request whose
// hostapiauth.Header does not match the secret of this sidecar gets 401, and
// no route handler runs (#3379).
//
// This check is what makes the caller the session of this sidecar. The role
// gates (requireCoordinator, callerHasCoordinatorRole, and the
// role-scoped handlers) read the start config of this sidecar, so they are
// correct only for a caller that passed this check. A sidecar with no secret
// refuses every request (hostapiauth.Matches).
func (s *Sidecar) requireHostAPICredential(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostapiauth.Matches(r.Header.Get(hostapiauth.Header), s.currentHostAPISecret()) {
			s.logger().Printf("sidecar: host-API: refused %s %s: no valid credential for session %q", r.Method, r.URL.Path, s.cfg.SessionName)
			b, _ := json.Marshal(map[string]string{"error": hostAPIRefusedMessage})
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write(b)
			return
		}
		next.ServeHTTP(w, r)
	})
}
