package server_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stearz/voidshell/internal/workspace"
	"golang.org/x/crypto/ssh"
	"k8s.io/client-go/tools/remotecommand"
)

type sessionBackend struct {
	fakeAttacher
	run     func(context.Context, []string, bool, io.Reader, io.Writer, io.Writer) (uint32, error)
	forward func(context.Context, workspace.Identity, workspace.StorageMode, uint16, io.Reader, io.Writer) error
}

func (b *sessionBackend) Attach(ctx context.Context, _ workspace.Identity, _ workspace.StorageMode, term string, tty bool, _ remotecommand.TerminalSizeQueue, in io.Reader, out, stderr io.Writer) (uint32, error) {
	return b.run(ctx, []string{"shell", term}, tty, in, out, stderr)
}
func (b *sessionBackend) Exec(ctx context.Context, _ workspace.Identity, _ workspace.StorageMode, cmd []string, tty bool, _ remotecommand.TerminalSizeQueue, in io.Reader, out, stderr io.Writer) (uint32, error) {
	return b.run(ctx, cmd, tty, in, out, stderr)
}
func (b *sessionBackend) Forward(ctx context.Context, id workspace.Identity, mode workspace.StorageMode, port uint16, in io.Reader, out io.Writer) error {
	return b.forward(ctx, id, mode, port, in, out)
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for backend")
		var zero T
		return zero
	}
}

func newSession(t *testing.T, c *ssh.Client) *ssh.Session {
	t.Helper()
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestExecStreamsExitStatusAndQuoting(t *testing.T) {
	called := make(chan []string, 1)
	b := &sessionBackend{run: func(_ context.Context, cmd []string, tty bool, in io.Reader, out, stderr io.Writer) (uint32, error) {
		called <- cmd
		if tty {
			return 1, errors.New("unexpected tty")
		}
		io.Copy(out, in)
		io.WriteString(stderr, "diagnostic")
		return 7, nil
	}}
	lifecycle := &fakeLifecycle{}
	addr, key := startTestServer(t, &fakeAuth{githubUser: "alice"}, lifecycle, b)
	s := newSession(t, connectSSH(t, addr, key, "dev"))
	var stdout, stderr strings.Builder
	s.Stdin, s.Stdout, s.Stderr = strings.NewReader("payload"), &stdout, &stderr
	command := `printf '%s' 'hello world' | cat`
	err := s.Run(command)
	var exit *ssh.ExitError
	if !errors.As(err, &exit) || exit.ExitStatus() != 7 {
		t.Fatalf("exit = %v, want 7", err)
	}
	cmd := receive(t, called)
	if len(cmd) != 3 || cmd[0] != "/bin/sh" || cmd[1] != "-c" || cmd[2] != command {
		t.Fatalf("command = %q", cmd)
	}
	if stdout.String() != "payload" || stderr.String() != "diagnostic" {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if len(lifecycle.deleteCalls) != 1 {
		t.Fatalf("cleanup count = %d", len(lifecycle.deleteCalls))
	}
}

func TestOverlappingConnectionsKeepWorkspaceUntilLastSession(t *testing.T) {
	started := make(chan string, 2)
	finishFirst, finishSecond := make(chan struct{}), make(chan struct{})
	b := &sessionBackend{run: func(ctx context.Context, cmd []string, _ bool, _ io.Reader, _, _ io.Writer) (uint32, error) {
		name := cmd[2]
		started <- name
		finish := finishFirst
		if name == "second" {
			finish = finishSecond
		}
		select {
		case <-finish:
			return 0, nil
		case <-ctx.Done():
			return 1, ctx.Err()
		}
	}}
	lifecycle := &fakeLifecycle{}
	addr, key := startTestServer(t, &fakeAuth{githubUser: "alice"}, lifecycle, b)
	first := newSession(t, connectSSH(t, addr, key, "persist.dev"))
	second := newSession(t, connectSSH(t, addr, key, "persist.dev"))
	if err := first.Start("first"); err != nil {
		t.Fatal(err)
	}
	receive(t, started)
	if err := second.Start("second"); err != nil {
		t.Fatal(err)
	}
	receive(t, started)
	close(finishFirst)
	if err := first.Wait(); err != nil {
		t.Fatal(err)
	}
	lifecycle.mu.Lock()
	ensures, deletes := len(lifecycle.ensureCalls), len(lifecycle.deleteCalls)
	lifecycle.mu.Unlock()
	if ensures != 1 || deletes != 0 {
		t.Fatalf("while second active: ensure=%d delete=%d", ensures, deletes)
	}
	close(finishSecond)
	if err := second.Wait(); err != nil {
		t.Fatal(err)
	}
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if len(lifecycle.deleteCalls) != 1 {
		t.Fatalf("delete=%d", len(lifecycle.deleteCalls))
	}
}

func TestDisconnectCancelsExecutionAndCleansPod(t *testing.T) {
	started := make(chan struct{}, 1)
	canceled := make(chan struct{}, 1)
	b := &sessionBackend{run: func(ctx context.Context, _ []string, _ bool, _ io.Reader, _, _ io.Writer) (uint32, error) {
		started <- struct{}{}
		<-ctx.Done()
		canceled <- struct{}{}
		return 1, ctx.Err()
	}}
	lifecycle := &fakeLifecycle{}
	addr, key := startTestServer(t, &fakeAuth{githubUser: "alice"}, lifecycle, b)
	c := connectSSH(t, addr, key, "dev")
	s := newSession(t, c)
	if err := s.Start("hold"); err != nil {
		t.Fatal(err)
	}
	receive(t, started)
	c.Close()
	receive(t, canceled)
	// Reconnecting must wait for cleanup and provision a fresh workspace.
	c = connectSSH(t, addr, key, "dev")
	s = newSession(t, c)
	if err := s.Start("hold"); err != nil {
		t.Fatal(err)
	}
	receive(t, started)
	lifecycle.mu.Lock()
	ensures, deletes := len(lifecycle.ensureCalls), len(lifecycle.deleteCalls)
	lifecycle.mu.Unlock()
	if ensures != 2 || deletes != 1 {
		t.Fatalf("reconnect: ensure=%d delete=%d", ensures, deletes)
	}
	c.Close()
	receive(t, canceled)
}

func TestSFTPSubsystemUsesBinaryStream(t *testing.T) {
	command := make(chan []string, 1)
	b := &sessionBackend{run: func(_ context.Context, cmd []string, tty bool, _ io.Reader, out, _ io.Writer) (uint32, error) {
		command <- cmd
		if tty {
			return 1, errors.New("sftp must not use a PTY")
		}
		_, err := out.Write([]byte{0, 0, 0, 5, 2, 0, 0, 0, 3})
		return 0, err
	}}
	addr, key := startTestServer(t, &fakeAuth{githubUser: "alice"}, &fakeLifecycle{}, b)
	c := connectSSH(t, addr, key, "dev")
	ch, reqs, err := c.OpenChannel("session", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	go ssh.DiscardRequests(reqs)
	if ok, err := ch.SendRequest("subsystem", true, ssh.Marshal(struct{ Name string }{"unknown"})); err != nil || ok {
		t.Fatalf("unknown subsystem: %v, %v", ok, err)
	}
	if ok, err := ch.SendRequest("subsystem", true, ssh.Marshal(struct{ Name string }{"sftp"})); err != nil || !ok {
		t.Fatalf("sftp: %v, %v", ok, err)
	}
	output, err := io.ReadAll(ch)
	if err != nil {
		t.Fatal(err)
	}

	cmd := receive(t, command)
	if len(cmd) != 3 || !strings.Contains(cmd[2], "exec \"$p\"") {
		t.Fatalf("sftp command=%q", cmd)
	}
	if string(output) != string([]byte{0, 0, 0, 5, 2, 0, 0, 0, 3}) {
		t.Fatalf("corrupt subsystem output: %q", output)
	}
}

func TestShellPassesTerminalPerSession(t *testing.T) {
	// The existing shell tests exercise the no-output shell lifecycle. Verify the
	// terminal name is passed per process rather than put on the shared pod.
	called := make(chan []string, 1)
	b := &sessionBackend{run: func(_ context.Context, cmd []string, tty bool, _ io.Reader, out, stderr io.Writer) (uint32, error) {
		if !tty || stderr != nil {
			return 1, errors.New("incorrect PTY streams")
		}
		called <- cmd
		return 0, nil
	}}
	addr, key := startTestServer(t, &fakeAuth{githubUser: "alice"}, &fakeLifecycle{}, b)
	s := newSession(t, connectSSH(t, addr, key, "dev"))
	if err := s.RequestPty("xterm-256color", 24, 80, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Shell(); err != nil {
		t.Fatal(err)
	}
	if err := s.Wait(); err != nil {
		t.Fatal(err)
	}
	if cmd := receive(t, called); cmd[1] != "xterm-256color" {
		t.Fatalf("term=%q", cmd)
	}
}

func TestForwardingRestrictedToSelectedWorkspace(t *testing.T) {
	ports := make(chan uint16, 1)
	b := &sessionBackend{forward: func(ctx context.Context, id workspace.Identity, mode workspace.StorageMode, port uint16, in io.Reader, out io.Writer) error {
		if id.GithubUser != "alice" || id.SSHUser != "dev" || mode != workspace.StoragePersistent {
			return errors.New("wrong workspace")
		}
		ports <- port
		_, err := io.Copy(out, in)
		return err
	}}
	lifecycle := &fakeLifecycle{}
	addr, key := startTestServer(t, &fakeAuth{githubUser: "alice"}, lifecycle, b)
	c := connectSSH(t, addr, key, "persist.dev")
	for _, target := range []string{"10.0.0.1:80", "example.com:80", "localhost:0", "localhost:65536"} {
		conn, err := c.Dial("tcp", target)
		if err == nil {
			conn.Close()
			t.Fatalf("accepted %s", target)
		}
	}
	lifecycle.mu.Lock()
	ensures := len(lifecycle.ensureCalls)
	lifecycle.mu.Unlock()
	if ensures != 0 {
		t.Fatal("rejected forwarding provisioned workspace")
	}
	conn, err := c.Dial("tcp", "localhost:3000")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if port := receive(t, ports); port != 3000 {
		t.Fatalf("port=%d", port)
	}
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	result := make([]byte, 5)
	if _, err := io.ReadFull(conn, result); err != nil {
		t.Fatal(err)
	}
	if string(result) != "hello" {
		t.Fatalf("echo=%q", result)
	}
}
