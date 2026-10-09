package sidecar

import (
	"context"

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
		Runner:        s.cfg.ContainerRunner,
		BuildExecutor: s.cfg.ContainerBuildExecutor,
		HostLimit:     limit,
	}
}

// containerCaller returns this sidecar's own session. No field comes from
// a request.
func (s *Sidecar) containerCaller() prismcontainer.Caller {
	return prismcontainer.Caller{
		SessionName: s.cfg.SessionName,
		InstanceID:  s.cfg.InstanceID,
		Worktree:    s.cfg.Worktree,
	}
}

// runContainer serves POST /container/run for this sidecar's session.
func (s *Sidecar) runContainer(ctx context.Context, req prismcontainer.RunRequest) prismcontainer.RunResult {
	res := prismcontainer.Run(ctx, s.containerDeps(), s.containerCaller(), req)
	s.logger().Printf("sidecar: host-API /container/run: image=%q exit=%d", req.Image, res.ExitCode)
	return res
}

// buildContainer serves POST /container/build for this sidecar's session.
func (s *Sidecar) buildContainer(ctx context.Context, req prismcontainer.BuildRequest) prismcontainer.BuildResult {
	res := prismcontainer.Build(ctx, s.containerDeps(), s.containerCaller(), req)
	s.logger().Printf("sidecar: host-API /container/build: image=%q exit=%d", res.Image, res.ExitCode)
	return res
}
