package server_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/stearz/voidshell/internal/server"
	"github.com/stearz/voidshell/internal/workspace"
)

// --- fakes ---

type fakeAuth struct {
	githubUser string
	err        error
}

func (f *fakeAuth) Authenticate(_ context.Context, _ ssh.PublicKey, _ string) (string, error) {
	return f.githubUser, f.err
}

type fakeLifecycle struct {
	mu          sync.Mutex
	ensureCalls []workspace.Identity
	deleteCalls []workspace.Identity
	ensureErr   error
}

func (f *fakeLifecycle) EnsureWorkspace(_ context.Context, id workspace.Identity) error {
	f.mu.Lock()
	f.ensureCalls = append(f.ensureCalls, id)
	f.mu.Unlock()
	return f.ensureErr
}

func (f *fakeLifecycle) DeletePod(_ context.Context, id workspace.Identity) error {
	f.mu.Lock()
	f.deleteCalls = append(f.deleteCalls, id)
	f.mu.Unlock()
	return nil
}

type fakeAttacher struct {
	mu          sync.Mutex
	attachCalls int
	err         error
}

func (f *fakeAttacher) Attach(_ context.Context, _ workspace.Identity, _ bool, _ remotecommand.TerminalSizeQueue, _ io.Reader, _, _ io.Writer) error {
	f.mu.Lock()
	f.attachCalls++
	f.mu.Unlock()
	return f.err
}

// --- helpers ---

// startTestServer creates a Server with a generated host key, starts it on a
// random local port, and returns the address and host public key.
func startTestServer(t *testing.T, auth server.Authenticator, lifecycle server.WorkspaceLifecycle, attacher server.PodAttacher) (addr string, hostPub ssh.PublicKey) {
	t.Helper()
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatalf("host signer: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(hostSigner, auth, lifecycle, attacher, log)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr = ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		ln.Close()
	})
	go srv.Serve(ctx, ln) //nolint:errcheck

	return addr, hostSigner.PublicKey()
}

// connectSSH dials the test server and returns an SSH client.
func connectSSH(t *testing.T, addr string, hostPub ssh.PublicKey, sshUser string) *ssh.Client {
	t.Helper()
	_, clientPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	clientSigner, err := ssh.NewSignerFromKey(clientPriv)
	if err != nil {
		t.Fatalf("client signer: %v", err)
	}

	cfg := &ssh.ClientConfig{
		User: sshUser,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(clientSigner)},
		HostKeyCallback: ssh.FixedHostKey(hostPub),
	}
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		t.Fatalf("SSH dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

// --- tests ---

// TestAuthFailureNoPodCreated verifies that when auth rejects the offered key,
// EnsureWorkspace is never called.
func TestAuthFailureNoPodCreated(t *testing.T) {
	auth := &fakeAuth{err: errors.New("unknown key")}
	lifecycle := &fakeLifecycle{}
	attacher := &fakeAttacher{}

	addr, hostPub := startTestServer(t, auth, lifecycle, attacher)

	_, clientPriv, _ := ed25519.GenerateKey(rand.Reader)
	clientSigner, _ := ssh.NewSignerFromKey(clientPriv)
	cfg := &ssh.ClientConfig{
		User: "testuser",
		Auth: []ssh.AuthMethod{ssh.PublicKeys(clientSigner)},
		HostKeyCallback: ssh.FixedHostKey(hostPub),
	}
	_, err := ssh.Dial("tcp", addr, cfg)
	if err == nil {
		t.Fatal("expected SSH handshake to fail, got nil error")
	}

	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if len(lifecycle.ensureCalls) != 0 {
		t.Errorf("EnsureWorkspace called %d times, want 0", len(lifecycle.ensureCalls))
	}
}

// TestSuccessfulShellSessionCallsDeletePod verifies that a complete shell
// session calls EnsureWorkspace once and DeletePod exactly once afterward.
func TestSuccessfulShellSessionCallsDeletePod(t *testing.T) {
	auth := &fakeAuth{githubUser: "octocat"}
	lifecycle := &fakeLifecycle{}
	attacher := &fakeAttacher{} // returns immediately → session ends cleanly

	addr, hostPub := startTestServer(t, auth, lifecycle, attacher)
	client := connectSSH(t, addr, hostPub, "myshell")

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := session.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatalf("RequestPty: %v", err)
	}
	if err := session.Shell(); err != nil {
		t.Fatalf("Shell: %v", err)
	}

	// Wait blocks until exit-status is received, which happens after DeletePod.
	session.Wait() //nolint:errcheck

	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if len(lifecycle.ensureCalls) != 1 {
		t.Errorf("EnsureWorkspace called %d times, want 1", len(lifecycle.ensureCalls))
	}
	if len(lifecycle.deleteCalls) != 1 {
		t.Errorf("DeletePod called %d times, want 1", len(lifecycle.deleteCalls))
	}
}

// TestEnsureWorkspaceFailureNoPodLeft verifies that when EnsureWorkspace fails,
// an error is written to the SSH client's stderr and Attach is never called.
func TestEnsureWorkspaceFailureNoPodLeft(t *testing.T) {
	auth := &fakeAuth{githubUser: "octocat"}
	lifecycle := &fakeLifecycle{ensureErr: fmt.Errorf("image pull backoff")}
	attacher := &fakeAttacher{}

	addr, hostPub := startTestServer(t, auth, lifecycle, attacher)
	client := connectSSH(t, addr, hostPub, "myshell")

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := session.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatalf("RequestPty: %v", err)
	}

	var stderrBuf strings.Builder
	session.Stderr = &stderrBuf

	if err := session.Shell(); err != nil {
		t.Fatalf("Shell: %v", err)
	}
	session.Wait() //nolint:errcheck

	if !strings.Contains(stderrBuf.String(), "workspace setup failed") {
		t.Errorf("stderr %q missing 'workspace setup failed'", stderrBuf.String())
	}
	attacher.mu.Lock()
	defer attacher.mu.Unlock()
	if attacher.attachCalls != 0 {
		t.Errorf("Attach called %d times, want 0", attacher.attachCalls)
	}
}

// TestWorkspaceIdentityFromAuth verifies that the workspace identity uses the
// authenticated GitHub username, not just the raw SSH username.
func TestWorkspaceIdentityFromAuth(t *testing.T) {
	auth := &fakeAuth{githubUser: "octocat"}
	lifecycle := &fakeLifecycle{}
	attacher := &fakeAttacher{}

	addr, hostPub := startTestServer(t, auth, lifecycle, attacher)
	client := connectSSH(t, addr, hostPub, "devbox")

	session, _ := client.NewSession()
	session.RequestPty("xterm", 24, 80, ssh.TerminalModes{}) //nolint:errcheck
	session.Shell()                                           //nolint:errcheck
	session.Wait()                                            //nolint:errcheck

	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if len(lifecycle.ensureCalls) == 0 {
		t.Fatal("EnsureWorkspace not called")
	}
	got := lifecycle.ensureCalls[0]
	if got.GithubUser != "octocat" {
		t.Errorf("GithubUser = %q, want %q", got.GithubUser, "octocat")
	}
	if got.SSHUser != "devbox" {
		t.Errorf("SSHUser = %q, want %q", got.SSHUser, "devbox")
	}
}
