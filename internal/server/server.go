// Package server implements the voidshell SSH server.
package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"

	"golang.org/x/crypto/ssh"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/stearz/voidshell/internal/workspace"
)

// Authenticator verifies SSH public keys against GitHub user key lists.
type Authenticator interface {
	Authenticate(ctx context.Context, key ssh.PublicKey, sshUser string) (string, error)
}

// WorkspaceLifecycle creates and cleans up workspace pods and PVCs.
type WorkspaceLifecycle interface {
	EnsureWorkspace(ctx context.Context, id workspace.Identity) error
	DeletePod(ctx context.Context, id workspace.Identity) error
}

// PodAttacher attaches stdin/stdout/stderr to a running workspace pod.
type PodAttacher interface {
	Attach(ctx context.Context, id workspace.Identity, tty bool, resizeQueue remotecommand.TerminalSizeQueue, stdin io.Reader, stdout, stderr io.Writer) error
}

// Server is the voidshell SSH server.
type Server struct {
	sshCfg    *ssh.ServerConfig
	lifecycle WorkspaceLifecycle
	attacher  PodAttacher
	log       *slog.Logger
}

// New creates a Server. hostKey is the SSH host key signer. auth authenticates
// incoming public keys. lifecycle creates and cleans up workspace pods.
// attacher connects the SSH streams to a running pod.
func New(hostKey ssh.Signer, auth Authenticator, lifecycle WorkspaceLifecycle, attacher PodAttacher, log *slog.Logger) *Server {
	s := &Server{
		lifecycle: lifecycle,
		attacher:  attacher,
		log:       log,
	}
	s.sshCfg = &ssh.ServerConfig{
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			ghUser, err := auth.Authenticate(context.Background(), key, conn.User())
			if err != nil {
				return nil, err
			}
			return &ssh.Permissions{
				Extensions: map[string]string{"github_user": ghUser},
			}, nil
		},
	}
	s.sshCfg.AddHostKey(hostKey)
	return s
}

// ListenAndServe starts a TCP listener on addr and calls Serve.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	return s.Serve(ctx, ln)
}

// Serve accepts connections on ln until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				s.log.Error("ssh: accept error", "error", err)
				continue
			}
		}
		go s.handleConn(ctx, conn)
	}
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	sshConn, chans, reqs, err := ssh.NewServerConn(conn, s.sshCfg)
	if err != nil {
		// Auth failure or protocol error; the auth callback already logged details.
		return
	}
	defer sshConn.Close()

	ghUser := sshConn.Permissions.Extensions["github_user"]
	sshUser := sshConn.User()
	s.log.Info("ssh: connection established",
		"github_user", ghUser,
		"ssh_user", sshUser,
		"remote", sshConn.RemoteAddr(),
	)

	id := workspace.New(ghUser, sshUser)
	go ssh.DiscardRequests(reqs)

	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			newChan.Reject(ssh.UnknownChannelType, "only session channels are supported")
			continue
		}
		go s.handleSession(ctx, newChan, id)
	}
}
