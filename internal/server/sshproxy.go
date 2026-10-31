package server

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/stearz/voidshell/internal/workspace"
	"golang.org/x/crypto/ssh"
)

// DirectSSHBackend uses the same workspace lease as Kubernetes exec. One backend
// transport per channel keeps disconnects and cancellation independent.
type DirectSSHBackend interface {
	DirectSSHEnabled() bool
	OpenWorkspaceSSH(context.Context, workspace.Identity, workspace.StorageMode) (*ssh.Client, error)
}

func (s *Server) proxySession(ctx context.Context, backend DirectSSHBackend, ch ssh.Channel, reqs <-chan *ssh.Request, id workspace.Identity, mode workspace.StorageMode) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Observe client channel closure even while provisioning or handshaking.
	pending := make(chan *ssh.Request, 16)
	go func() {
		defer close(pending)
		defer cancel()
		for req := range reqs {
			select {
			case pending <- req:
			case <-ctx.Done():
				return
			}
		}
	}()
	release, err := s.acquireWorkspace(ctx, id, mode)
	if err != nil {
		s.log.Error("session: workspace setup failed", "workspace", id.WorkspaceID(), "error", err)
		fmt.Fprintf(ch.Stderr(), "voidshell: workspace setup failed: %v\n", err)
		sendExitStatus(ch, 1)
		return
	}
	defer release()
	client, err := backend.OpenWorkspaceSSH(ctx, id, mode)
	if err != nil {
		s.log.Error("session: workspace SSH failed", "workspace", id.WorkspaceID(), "error", err)
		fmt.Fprintf(ch.Stderr(), "voidshell: workspace SSH failed: %v\n", err)
		release()
		sendExitStatus(ch, 1)
		return
	}
	defer client.Close()
	stop := context.AfterFunc(ctx, func() { client.Close() })
	defer stop()
	remote, remoteRequests, err := client.OpenChannel("session", nil)
	if err != nil {
		s.log.Error("session: workspace SSH channel failed", "workspace", id.WorkspaceID(), "error", err)
		fmt.Fprintf(ch.Stderr(), "voidshell: workspace SSH channel failed: %v\n", err)
		release()
		sendExitStatus(ch, 1)
		return
	}
	defer remote.Close()
	go func() { io.Copy(remote, ch); remote.CloseWrite() }()
	var output sync.WaitGroup
	output.Add(2)
	go func() { defer output.Done(); io.Copy(ch, remote) }()
	go func() { defer output.Done(); io.Copy(ch.Stderr(), remote.Stderr()) }()
	outputDone := make(chan struct{})
	go func() {
		output.Wait()
		// Output EOF is independent of process exit. The backend may still be
		// reading stdin, which must remain open after both output streams end.
		ch.CloseWrite()
		close(outputDone)
	}()
	go func() {
		started, pty := false, false
		for req := range pending {
			if !validProxyRequest(req, started, pty) {
				if req.WantReply {
					req.Reply(false, nil)
				}
				continue
			}
			ok, err := remote.SendRequest(req.Type, req.WantReply, req.Payload)
			if err != nil {
				cancel()
				return
			}
			if !req.WantReply || ok {
				switch req.Type {
				case "pty-req":
					pty = true
				case "shell", "exec", "subsystem":
					started = true
				}
			}
			if req.WantReply {
				req.Reply(ok, nil)
			}
		}
	}()
	// Preserve exit-status/exit-signal payloads exactly, but release the workspace
	// before reporting completion so an immediate reconnect cannot reuse a dying pod.
	var exit *ssh.Request
	for req := range remoteRequests {
		if req.Type == "exit-status" || req.Type == "exit-signal" {
			exit = req
		}
		if req.WantReply {
			req.Reply(false, nil)
		}
	}
	<-outputDone
	release()
	if exit != nil {
		ch.SendRequest(exit.Type, false, exit.Payload)
	} else if ctx.Err() == nil {
		sendExitStatus(ch, 1)
	}
}

func validProxyRequest(req *ssh.Request, started, pty bool) bool {
	switch req.Type {
	case "pty-req":
		var msg ptyRequestMsg
		return !started && !pty && ssh.Unmarshal(req.Payload, &msg) == nil && !strings.ContainsRune(msg.Term, 0)
	case "shell":
		return !started && len(req.Payload) == 0
	case "exec":
		var msg struct{ Command string }
		return !started && ssh.Unmarshal(req.Payload, &msg) == nil && !strings.ContainsRune(msg.Command, 0)
	case "subsystem":
		var msg struct{ Name string }
		return !started && !pty && ssh.Unmarshal(req.Payload, &msg) == nil && msg.Name == "sftp"
	case "window-change":
		var msg windowChangeMsg
		return pty && ssh.Unmarshal(req.Payload, &msg) == nil
	case "signal":
		var msg struct{ Signal string }
		return started && ssh.Unmarshal(req.Payload, &msg) == nil && !strings.ContainsRune(msg.Signal, 0)
	default:
		return false
	}
}
