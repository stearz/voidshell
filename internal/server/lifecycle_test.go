package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stearz/voidshell/internal/workspace"
)

type controlledLifecycle struct {
	mu               sync.Mutex
	ensures, deletes int
	ensure           func(context.Context) error
	delete           func(context.Context) error
}

func (l *controlledLifecycle) EnsureWorkspace(ctx context.Context, _ workspace.Identity, _ workspace.StorageMode) error {
	l.mu.Lock()
	l.ensures++
	l.mu.Unlock()
	if l.ensure != nil {
		return l.ensure(ctx)
	}
	return nil
}
func (l *controlledLifecycle) DeletePod(ctx context.Context, _ workspace.Identity, _ workspace.StorageMode) error {
	l.mu.Lock()
	l.deletes++
	l.mu.Unlock()
	if l.delete != nil {
		return l.delete(ctx)
	}
	return nil
}
func leaseServer(l WorkspaceLifecycle) *Server {
	return &Server{lifecycle: l, workspaces: make(map[string]*workspaceLease), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}
func await[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
		var zero T
		return zero
	}
}

func TestConcurrentStartupUsesOnePod(t *testing.T) {
	started := make(chan struct{}, 1)
	proceed := make(chan struct{})
	l := &controlledLifecycle{ensure: func(ctx context.Context) error {
		started <- struct{}{}
		select {
		case <-proceed:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	s := leaseServer(l)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id := workspace.New("alice", "dev")
	releases := make(chan func(), 2)
	errs := make(chan error, 2)
	acquire := func() {
		release, err := s.acquireWorkspace(ctx, id, workspace.StorageEphemeral)
		if err != nil {
			errs <- err
			return
		}
		releases <- release
	}
	go acquire()
	await(t, started)
	go acquire()
	close(proceed)
	first, second := await(t, releases), await(t, releases)
	first()
	l.mu.Lock()
	ensures, deletes := l.ensures, l.deletes
	l.mu.Unlock()
	if ensures != 1 || deletes != 0 {
		t.Fatalf("active workspace: ensure=%d delete=%d", ensures, deletes)
	}
	second()
	second() // A duplicate release must not remove another session's workspace.
	l.mu.Lock()
	deletes = l.deletes
	l.mu.Unlock()
	if deletes != 1 {
		t.Fatalf("cleanup=%d", deletes)
	}
	s.mu.Lock()
	entries := len(s.workspaces)
	s.mu.Unlock()
	if entries != 0 {
		t.Fatalf("idle lease entries=%d", entries)
	}
	select {
	case err := <-errs:
		t.Fatal(err)
	default:
	}
}

func TestDisconnectDuringSetupCleansPartialPod(t *testing.T) {
	started := make(chan struct{}, 1)
	l := &controlledLifecycle{ensure: func(ctx context.Context) error {
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}}
	s := leaseServer(l)
	s.idleTimeout = 30 * time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := s.acquireWorkspace(ctx, workspace.New("alice", "dev"), workspace.StorageEphemeral)
		result <- err
	}()
	await(t, started)
	cancel()
	if err := await(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("setup=%v", err)
	}
	l.mu.Lock()
	deletes := l.deletes
	l.mu.Unlock()
	if deletes != 1 {
		t.Fatalf("partial pod cleanup=%d", deletes)
	}
}

func TestReconnectWaitsForDeletion(t *testing.T) {
	deleting := make(chan struct{}, 4)
	proceed := make(chan struct{})
	l := &controlledLifecycle{delete: func(ctx context.Context) error {
		deleting <- struct{}{}
		select {
		case <-proceed:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	s := leaseServer(l)
	id := workspace.New("alice", "dev")
	first, err := s.acquireWorkspace(context.Background(), id, workspace.StorageEphemeral)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() { first(); close(released) }()
	await(t, deleting)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reacquired := make(chan func(), 1)
	go func() {
		release, err := s.acquireWorkspace(ctx, id, workspace.StorageEphemeral)
		if err == nil {
			reacquired <- release
		}
	}()
	// Another workspace must remain usable while deletion is blocked.
	other, err := s.acquireWorkspace(ctx, id, workspace.StoragePersistent)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-reacquired:
		t.Fatal("reused a pod during deletion")
	default:
	}
	close(proceed)
	await(t, released)
	second := await(t, reacquired)
	second()
	other()
}

func (l *controlledLifecycle) deleteCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.deletes
}
func (l *controlledLifecycle) ensureCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ensures
}
func (s *Server) workspaceCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.workspaces)
}

// Virtual time exercises production-length idle windows without wall-clock sleeps.
func TestIdleTimeoutRetainsAndResetsWorkspace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &controlledLifecycle{}
		s := leaseServer(l)
		s.idleTimeout = 30 * time.Minute
		id := workspace.New("alice", "dev")
		acquire := func() func() {
			t.Helper()
			release, err := s.acquireWorkspace(context.Background(), id, workspace.StorageEphemeral)
			if err != nil {
				t.Fatal(err)
			}
			return release
		}
		first, second := acquire(), acquire()
		first()
		time.Sleep(time.Hour)
		synctest.Wait()
		if l.deleteCount() != 0 {
			t.Fatal("deleted while another channel was active")
		}
		second()
		second() // Duplicate release must not shorten the idle window.
		time.Sleep(29 * time.Minute)
		synctest.Wait()
		if l.deleteCount() != 0 || s.workspaceCount() != 1 {
			t.Fatal("workspace not retained")
		}
		resumed := acquire()
		time.Sleep(2 * time.Minute) // Cross the original deadline while active.
		synctest.Wait()
		if l.deleteCount() != 0 {
			t.Fatal("old deadline deleted resumed workspace")
		}
		resumed()
		time.Sleep(29 * time.Minute)
		synctest.Wait()
		if l.deleteCount() != 0 {
			t.Fatal("idle deadline was not reset")
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if l.deleteCount() != 1 || s.workspaceCount() != 0 {
			t.Fatal("expired workspace not cleaned exactly once")
		}
		s.cleanupIdleWorkspaces()
	})
}

func TestIdleTimeoutIsPerWorkspaceAndStorageMode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &controlledLifecycle{}
		s := leaseServer(l)
		s.idleTimeout = 30 * time.Minute
		id := workspace.New("alice", "dev")
		for _, mode := range []workspace.StorageMode{workspace.StorageEphemeral, workspace.StoragePersistent} {
			release, err := s.acquireWorkspace(context.Background(), id, mode)
			if err != nil {
				t.Fatal(err)
			}
			release()
			time.Sleep(15 * time.Minute)
		}
		synctest.Wait()
		if l.deleteCount() != 1 || s.workspaceCount() != 1 {
			t.Fatal("workspace deadlines were not independent")
		}
		s.cleanupIdleWorkspaces()
		if l.deleteCount() != 2 || s.workspaceCount() != 0 {
			t.Fatal("shutdown did not clean retained workspace")
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		if l.deleteCount() != 2 {
			t.Fatal("timer deleted again after shutdown")
		}
	})
}

func TestIdleExpirySerializesReconnectWithDeletion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		deleting := make(chan struct{})
		proceed := make(chan struct{})
		l := &controlledLifecycle{delete: func(context.Context) error {
			close(deleting)
			<-proceed
			return nil
		}}
		s := leaseServer(l)
		s.idleTimeout = 30 * time.Minute
		id := workspace.New("alice", "dev")
		release, err := s.acquireWorkspace(context.Background(), id, workspace.StorageEphemeral)
		if err != nil {
			t.Fatal(err)
		}
		release()
		time.Sleep(30 * time.Minute)
		<-deleting
		var resumed func()
		go func() {
			var err error
			resumed, err = s.acquireWorkspace(context.Background(), id, workspace.StorageEphemeral)
			if err != nil {
				panic(err)
			}
		}()
		synctest.Wait()
		if l.ensureCount() != 1 {
			t.Fatal("reprovisioned during deletion")
		}
		close(proceed)
		synctest.Wait()
		if resumed == nil || l.ensureCount() != 2 {
			t.Fatal("reconnect did not provision after deletion")
		}
		l.delete = nil
		resumed()
		s.cleanupIdleWorkspaces()
	})
}

func TestIdleShutdownDrainsExpiredTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &controlledLifecycle{}
		s := leaseServer(l)
		s.idleTimeout = 30 * time.Minute
		id := workspace.New("alice", "dev")
		release, err := s.acquireWorkspace(context.Background(), id, workspace.StorageEphemeral)
		if err != nil {
			t.Fatal(err)
		}
		release()
		lease := s.workspaces[id.PodNameFor(workspace.StorageEphemeral)]
		<-lease.gate
		time.Sleep(30 * time.Minute)
		synctest.Wait() // Expiry callback is already waiting for the gate.
		go s.cleanupIdleWorkspaces()
		synctest.Wait()
		lease.gate <- struct{}{}
		synctest.Wait()
		if l.deleteCount() != 1 || s.workspaceCount() != 0 {
			t.Fatal("shutdown/expiry race did not clean exactly once")
		}
	})
}
