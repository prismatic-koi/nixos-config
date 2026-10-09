package sidecar

import (
	"context"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/prismcontainer"
)

// runContainer serves POST /container/run for this sidecar's session.
func (s *Sidecar) runContainer(ctx context.Context, req prismcontainer.RunRequest) prismcontainer.RunResult {
	limit := s.cfg.ContainerHostLimit
	if limit == 0 {
		limit = config.LoadFresh().ContainerHostLimit
	}
	caller := prismcontainer.Caller{
		SessionName: s.cfg.SessionName,
		InstanceID:  s.cfg.InstanceID,
		Worktree:    s.cfg.Worktree,
	}
	res := prismcontainer.Run(ctx, prismcontainer.Deps{Runner: s.cfg.ContainerRunner, HostLimit: limit}, caller, req)
	s.logger().Printf("sidecar: host-API /container/run: image=%q exit=%d", req.Image, res.ExitCode)
	return res
}
