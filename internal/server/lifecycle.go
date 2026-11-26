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
	key     string
	gate    chan struct{}
	users   int         // protected by Server.mu
	active  int         // protected by gate
	timer   *time.Timer // protected by gate; holds one users reference
	cleanup func()      // protected by gate
	present bool        // protected by gate
}

func (s *Server) acquireWorkspace(ctx context.Context, id workspace.Identity, mode workspace.StorageMode) (func(), error) {
	key := id.PodNameFor(mode)
	s.mu.Lock()
	l := s.workspaces[key]
	if l == nil {
		l = &workspaceLease{key: key, gate: make(chan struct{}, 1)}
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
	// Cancel the old deadline while holding the same gate used by expiry.
	// A callback already running checks its timer identity before deleting.
	if l.timer != nil {
		if l.timer.Stop() {
			drop()
			s.idleTimers.Done()
		}
		l.timer = nil
	}
	l.cleanup = func() { cleanup(); l.present = false }
	if l.active == 0 {
		if err := s.lifecycle.EnsureWorkspace(ctx, id, mode); err != nil {
			// Setup can fail after creating a pod (for example image pull failure).
			l.cleanup()
			l.gate <- struct{}{}
			drop()
			return nil, err
		}
	}
	l.present = true
	l.active++
	l.gate <- struct{}{}
	var once sync.Once
	return func() {
		once.Do(func() {
			<-l.gate
			l.active--
			if l.active == 0 {
				if s.idleTimeout == 0 {
					l.cleanup()
				} else {
					s.mu.Lock()
					l.users++ // The timer owns the lease until stopped or completed.
					s.mu.Unlock()
					s.idleTimers.Add(1)
					var timer *time.Timer
					timer = time.AfterFunc(s.idleTimeout, func() {
						defer s.idleTimers.Done()
						<-l.gate
						if l.timer == timer {
							l.timer = nil
							s.log.Info("workspace: idle timeout expired", "workspace", key)
							l.cleanup()
						}
						l.gate <- struct{}{}
						drop()
					})
					l.timer = timer
					s.log.Info("workspace: idle cleanup scheduled", "workspace", key, "timeout", s.idleTimeout)
				}
			}
			l.gate <- struct{}{}
			drop()
		})
	}, nil
}

// Called only after all connections and their channel handlers have finished.
// Shutdown bypasses idle deadlines and waits for any expiry already in flight.
func (s *Server) cleanupIdleWorkspaces() {
	s.mu.Lock()
	leases := make([]*workspaceLease, 0, len(s.workspaces))
	for _, l := range s.workspaces {
		leases = append(leases, l)
	}
	s.mu.Unlock()
	var cleanup sync.WaitGroup
	for _, l := range leases {
		cleanup.Add(1)
		go func() {
			defer cleanup.Done()
			<-l.gate
			if l.timer != nil && l.timer.Stop() {
				s.mu.Lock()
				l.users--
				// All connections have finished; only timer callbacks can still hold users.
				if l.users == 0 {
					delete(s.workspaces, l.key)
				}
				s.mu.Unlock()
				s.idleTimers.Done()
			}
			l.timer = nil
			if l.present {
				l.cleanup()
			}
			l.gate <- struct{}{}
		}()
	}
	cleanup.Wait()
	s.idleTimers.Wait()
}
