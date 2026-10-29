package server

import (
	"context"

	"golang.org/x/crypto/ssh"

	"github.com/stearz/voidshell/internal/workspace"
)

type directTCPIPMsg struct {
	Host       string
	Port       uint32
	OriginHost string
	OriginPort uint32
}

func (s *Server) handleDirectTCPIP(ctx context.Context, newChan ssh.NewChannel, id workspace.Identity, mode workspace.StorageMode) {
	var msg directTCPIPMsg
	if ssh.Unmarshal(newChan.ExtraData(), &msg) != nil || msg.Port == 0 || msg.Port > 65535 {
		newChan.Reject(ssh.Prohibited, "invalid forwarding request")
		return
	}
	// No DNS resolution or dialing in the proxy's network namespace.
	if msg.Host != "localhost" && msg.Host != "127.0.0.1" && msg.Host != "::1" {
		newChan.Reject(ssh.Prohibited, "forwarding is restricted to workspace loopback")
		return
	}
	forwarder, ok := s.attacher.(PodForwarder)
	if !ok {
		newChan.Reject(ssh.Prohibited, "port forwarding unavailable")
		return
	}
	// Accept before provisioning so client channel closure cancels setup as well.
	ch, reqs, err := newChan.Accept()
	if err != nil {
		return
	}
	defer ch.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { ssh.DiscardRequests(reqs); cancel() }()
	release, err := s.acquireWorkspace(ctx, id, mode)
	if err != nil {
		s.log.Error("forward: workspace setup failed", "error", err)
		return
	}
	defer release()
	if err := forwarder.Forward(ctx, id, mode, uint16(msg.Port), ch, ch); err != nil && ctx.Err() == nil {
		s.log.Info("forward: stream ended", "workspace", id.WorkspaceID(), "port", msg.Port, "error", err)
	}
}
