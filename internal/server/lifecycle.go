package server

import (
	"context"
	"sync"
	"time"

	"github.com/stearz/voidshell/internal/workspace"
)

// A lease serializes creation/deletion for one workspace. users includes callers
// waiting for the gate, so an entry cannot be removed while someone still uses it.
// Coordination is process-local: run a single voidshell replica.
type workspaceLease struct {
	gate   chan struct{}
	users  int // protected by Server.mu
	active int // protected by gate
}

func (s *Server) acquireWorkspace(ctx context.Context, id workspace.Identity, mode workspace.StorageMode) (func(), error) {
	key := id.PodNameFor(mode)
	s.mu.Lock()
	l := s.workspaces[key]
	if l == nil {
		l = &workspaceLease{gate: make(chan struct{}, 1)}
		l.gate <- struct{}{}
		s.workspaces[key] = l
	}
	l.users++
	s.mu.Unlock()
	drop := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		l.users--
		if l.users == 0 {
			delete(s.workspaces, key)
		}
	}
	select {
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	case <-l.gate:
	}
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.lifecycle.DeletePod(cleanupCtx, id, mode); err != nil {
			s.log.Error("workspace: pod cleanup failed", "workspace", key, "error", err)
		}
	}
	if err := ctx.Err(); err != nil {
		l.gate <- struct{}{}
		drop()
		return nil, err
	}
	if l.active == 0 {
		if err := s.lifecycle.EnsureWorkspace(ctx, id, mode); err != nil {
			// Setup can fail after creating a pod (for example image pull failure).
			cleanup()
			l.gate <- struct{}{}
			drop()
			return nil, err
		}
	}
	l.active++
	l.gate <- struct{}{}
	var once sync.Once
	return func() {
		once.Do(func() {
			<-l.gate
			l.active--
			if l.active == 0 {
				cleanup()
			}
			l.gate <- struct{}{}
			drop()
		})
	}, nil
}
