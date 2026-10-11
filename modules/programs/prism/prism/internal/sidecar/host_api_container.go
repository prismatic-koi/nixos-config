package sidecar

import (
	"context"
	"net/http"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/prismcontainer"
)

// containerDeps returns the prism container dependencies of this sidecar.
func (s *Sidecar) containerDeps() prismcontainer.Deps {
	limit := s.cfg.ContainerHostLimit
	if limit == 0 {
		limit = config.LoadFresh().ContainerHostLimit
	}
	return prismcontainer.Deps{
		Runner:                s.cfg.ContainerRunner,
		BuildExecutor:         s.cfg.ContainerBuildExecutor,
		HostLimit:             limit,
		MachineMountAllowlist: config.LoadFresh().MachineMountAllowlist(),
	}
}

// containerCaller returns this sidecar's own session. No field comes from
// a request.
func (s *Sidecar) containerCaller() prismcontainer.Caller {
	return prismcontainer.Caller{
		SessionName:      s.cfg.SessionName,
		InstanceID:       s.cfg.InstanceID,
		Worktree:         s.cfg.Worktree,
		WorktreeReadOnly: s.cfg.WorktreeReadOnly,
	}
}

// runContainer serves POST /container/run for this sidecar's session.
func (s *Sidecar) runContainer(ctx context.Context, req prismcontainer.RunRequest) prismcontainer.RunResult {
	res := prismcontainer.Run(ctx, s.containerDeps(), s.containerCaller(), req)
	s.logger().Printf("sidecar: host-API /container/run: image=%q exit=%d", req.Image, res.ExitCode)
	return res
}

// containerRoute returns the handler of a service-container endpoint: POST
// only, a strict JSON body of type Req, and HTTP 200 with the result for
// every decision. writeJSON is the response writer of hostAPIHandler.
func containerRoute[Req any](writeJSON func(http.ResponseWriter, int, any), serve func(context.Context, Req) any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		var req Req
		if status, err := decodeRequestJSON(w, r, &req, defaultMaxBodyBytes, false); err != nil {
			writeJSON(w, status, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, serve(r.Context(), req))
	}
}

// containerService logs the result of a service-container request.
func (s *Sidecar) containerService(command string, res any) any {
	code := -1
	switch r := res.(type) {
	case prismcontainer.StartResult:
		code = r.ExitCode
	case prismcontainer.RunResult:
		code = r.ExitCode
	case prismcontainer.ListResult:
		code = r.ExitCode
	}
	s.logger().Printf("sidecar: host-API /container/%s: exit=%d", command, code)
	return res
}

// buildContainer serves POST /container/build for this sidecar's session.
func (s *Sidecar) buildContainer(ctx context.Context, req prismcontainer.BuildRequest) prismcontainer.BuildResult {
	res := prismcontainer.Build(ctx, s.containerDeps(), s.containerCaller(), req)
	s.logger().Printf("sidecar: host-API /container/build: image=%q exit=%d", res.Image, res.ExitCode)
	return res
}
