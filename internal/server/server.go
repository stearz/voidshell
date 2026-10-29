// Package server implements the voidshell SSH server.
package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/stearz/voidshell/internal/workspace"
)

// Authenticator verifies SSH public keys against GitHub user key lists.
type Authenticator interface {
	Authenticate(ctx context.Context, key ssh.PublicKey, sshUser string) (string, error)
}

// WorkspaceLifecycle creates and cleans up workspace pods and their optional PVCs.
type WorkspaceLifecycle interface {
	EnsureWorkspace(ctx context.Context, id workspace.Identity, mode workspace.StorageMode) error
	DeletePod(ctx context.Context, id workspace.Identity, mode workspace.StorageMode) error
}

// PodAttacher starts independent shell and command processes in a workspace pod.
type PodAttacher interface {
	Attach(ctx context.Context, id workspace.Identity, mode workspace.StorageMode, term string, tty bool, resizeQueue remotecommand.TerminalSizeQueue, stdin io.Reader, stdout, stderr io.Writer) (uint32, error)
	Exec(ctx context.Context, id workspace.Identity, mode workspace.StorageMode, command []string, tty bool, resizeQueue remotecommand.TerminalSizeQueue, stdin io.Reader, stdout, stderr io.Writer) (uint32, error)
}

// PodForwarder connects only to a port in the selected workspace pod.
type PodForwarder interface {
	Forward(ctx context.Context, id workspace.Identity, mode workspace.StorageMode, port uint16, stdin io.Reader, stdout io.Writer) error
}

// Server is the voidshell SSH server.
type Server struct {
	sshCfg     *ssh.ServerConfig
	lifecycle  WorkspaceLifecycle
	attacher   PodAttacher
	log        *slog.Logger
	mu         sync.Mutex
	workspaces map[string]*workspaceLease
}

// New creates a Server. hostKey is the SSH host key signer. auth authenticates
// incoming public keys. lifecycle creates and cleans up workspace pods.
// attacher connects the SSH streams to a running pod.
func New(hostKey ssh.Signer, auth Authenticator, lifecycle WorkspaceLifecycle, attacher PodAttacher, log *slog.Logger) *Server {
	s := &Server{
		lifecycle:  lifecycle,
		attacher:   attacher,
		log:        log,
		workspaces: make(map[string]*workspaceLease),
	}
	s.sshCfg = &ssh.ServerConfig{
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if err := validateSSHUsername(conn.User()); err != nil {
				return nil, err
			}
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
	var connections sync.WaitGroup
	defer connections.Wait()
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
		connections.Add(1)
		go func() {
			defer connections.Done()
			s.handleConn(ctx, conn)
		}()
	}
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	// Bound unauthenticated connections, including probes that never send SSH data.
	conn.SetDeadline(time.Now().Add(15 * time.Second))

	sshConn, chans, reqs, err := ssh.NewServerConn(conn, s.sshCfg)
	if err != nil {
		// Auth failure or protocol error; the auth callback already logged details.
		return
	}
	defer sshConn.Close()
	conn.SetDeadline(time.Time{})
	go func() {
		sshConn.Wait()
		cancel()
	}()

	ghUser := sshConn.Permissions.Extensions["github_user"]
	sshUser := sshConn.User()
	s.log.Info("ssh: connection established",
		"github_user", ghUser,
		"ssh_user", sshUser,
		"remote", sshConn.RemoteAddr(),
	)

	selector := workspace.ParseSelector(sshUser)
	id := workspace.New(ghUser, selector.Name)
	s.log.Info("ssh: workspace selected",
		"github_user", ghUser,
		"ssh_user", sshUser,
		"workspace", selector.Name,
		"storage_mode", selector.Mode,
	)
	go handleGlobalRequests(reqs)

	var channels sync.WaitGroup
	for newChan := range chans {
		if newChan.ChannelType() != "session" && newChan.ChannelType() != "direct-tcpip" {
			newChan.Reject(ssh.UnknownChannelType, "unsupported channel type")
			continue
		}
		channels.Add(1)
		go func() {
			defer channels.Done()
			if newChan.ChannelType() == "direct-tcpip" {
				s.handleDirectTCPIP(ctx, newChan, id, selector.Mode)
			} else {
				s.handleSession(ctx, newChan, id, selector.Mode)
			}
		}()
	}
	cancel()
	channels.Wait()
}

func handleGlobalRequests(reqs <-chan *ssh.Request) {
	for req := range reqs {
		if req.WantReply {
			// Remote forwarding and other extensions remain unsupported.
			req.Reply(req.Type == "keepalive@openssh.com", nil)
		}
	}
}
