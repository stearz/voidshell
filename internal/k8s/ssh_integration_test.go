package k8s

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stearz/voidshell/internal/server"
	"github.com/stearz/voidshell/internal/workspace"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type integrationAuth struct{}

func (integrationAuth) Authenticate(context.Context, ssh.PublicKey, string) (string, error) {
	return "alice", nil
}

// Run with VOIDSHELL_TEST_SSH_IMAGE=voidshell-52-sshd:test after building
// testdata/Dockerfile.ssh-test from the repository root. Requires Docker and sftp.
// Port 2222 on localhost must be available. No Kubernetes cluster is touched.
func TestDirectSSHOpenSSH(t *testing.T) {
	image := os.Getenv("VOIDSHELL_TEST_SSH_IMAGE")
	if image == "" {
		t.Skip("set VOIDSHELL_TEST_SSH_IMAGE to run Docker/OpenSSH integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	m, id, pod := sshFixture(t)
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "workspace", Ready: true}}
	if _, err := m.client.CoreV1().Pods("test").UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	secret, err := m.client.CoreV1().Secrets("test").Get(ctx, pod.Annotations["voidshell.io/ssh-secret"], metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"host-key", "authorized_keys"} {
		if err := os.WriteFile(filepath.Join(dir, name), secret.Data[name], 0644); err != nil {
			t.Fatal(err)
		}
	}
	docker := func(t *testing.T, args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	container := docker(t, "run", "--rm", "-d", "--cap-drop=ALL", "--security-opt=no-new-privileges", "-e", "PATH=/test-bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "-p", "127.0.0.1:2222:2222", "-v", dir+":/run/voidshell-ssh:ro", image)
	defer func() { exec.Command("docker", "rm", "-f", container).Run() }()
	// Wait for the SSH handshake rather than relying on a container-start delay.
	var backend *ssh.Client
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		backend, err = m.OpenWorkspaceSSH(ctx, id, workspace.StorageEphemeral)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("workspace SSH startup: %v; logs: %s", err, docker(t, "logs", container))
	}
	backend.Close()
	// Verify both host-key pinning and client authentication fail closed.
	t.Run("host key mismatch", func(t *testing.T) {
		saved := secret.Data["host-key"]
		replacement, _, err := generateSSHKey()
		if err != nil {
			t.Fatal(err)
		}
		secret.Data["host-key"] = replacement
		m.client.CoreV1().Secrets("test").Update(ctx, secret, metav1.UpdateOptions{})
		if err := m.prepareSSH(ctx, id, workspace.StorageEphemeral); err != nil {
			t.Fatal(err)
		}
		if c, err := m.OpenWorkspaceSSH(ctx, id, workspace.StorageEphemeral); err == nil {
			c.Close()
			t.Fatal("accepted wrong host key")
		}
		secret.Data["host-key"] = saved
		m.client.CoreV1().Secrets("test").Update(ctx, secret, metav1.UpdateOptions{})
		if err := m.prepareSSH(ctx, id, workspace.StorageEphemeral); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("wrong client key", func(t *testing.T) {
		saved := secret.Data["client-key"]
		replacement, _, err := generateSSHKey()
		if err != nil {
			t.Fatal(err)
		}
		secret.Data["client-key"] = replacement
		m.client.CoreV1().Secrets("test").Update(ctx, secret, metav1.UpdateOptions{})
		if err := m.prepareSSH(ctx, id, workspace.StorageEphemeral); err != nil {
			t.Fatal(err)
		}
		if c, err := m.OpenWorkspaceSSH(ctx, id, workspace.StorageEphemeral); err == nil {
			c.Close()
			t.Fatal("accepted wrong client key")
		}
		secret.Data["client-key"] = saved
		m.client.CoreV1().Secrets("test").Update(ctx, secret, metav1.UpdateOptions{})
		if err := m.prepareSSH(ctx, id, workspace.StorageEphemeral); err != nil {
			t.Fatal(err)
		}
	})
	outerPEM, _, err := generateSSHKey()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(outerPEM)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	clientFiles := t.TempDir()
	clientKeyPath := filepath.Join(clientFiles, "client")
	clientKnownHosts := filepath.Join(clientFiles, "known_hosts")
	if err := os.WriteFile(clientKeyPath, outerPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(clientKnownHosts, []byte(fmt.Sprintf("[127.0.0.1]:%d %s", ln.Addr().(*net.TCPAddr).Port, ssh.MarshalAuthorizedKey(signer.PublicKey()))), 0600); err != nil {
		t.Fatal(err)
	}
	srv := server.New(signer, integrationAuth{}, m, m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	serverCtx, stopServer := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(serverCtx, ln) }()
	defer func() {
		stopServer()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	client, err := ssh.Dial("tcp", ln.Addr().String(), &ssh.ClientConfig{User: "dev", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.FixedHostKey(signer.PublicKey()), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	// Keep a lease across the subtests, matching overlapping shell/file transfers.
	keeper, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := keeper.Start("sleep 120"); err != nil {
		t.Fatal(err)
	}
	defer keeper.Close()
	t.Run("exec streams and exit", func(t *testing.T) {
		session, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		var stdout, stderr bytes.Buffer
		session.Stdin = strings.NewReader("payload")
		session.Stdout = &stdout
		session.Stderr = &stderr
		err = session.Run("cat; printf diagnostic >&2; exit 7")
		exit, ok := err.(*ssh.ExitError)
		if !ok || exit.ExitStatus() != 7 || stdout.String() != "payload" || stderr.String() != "diagnostic" {
			t.Fatalf("exit=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
		}
	})
	t.Run("output EOF before input EOF", func(t *testing.T) {
		session, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		stdin, err := session.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := session.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		stderr, err := session.StderrPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Start("printf marker; printf diagnostic >&2; exec 1>&- 2>&-; cat >/home/workspace/eof-input"); err != nil {
			t.Fatal(err)
		}
		type result struct {
			data []byte
			err  error
		}
		outDone, errDone := make(chan result, 1), make(chan result, 1)
		go func() { data, err := io.ReadAll(stdout); outDone <- result{data, err} }()
		go func() { data, err := io.ReadAll(stderr); errDone <- result{data, err} }()
		for _, stream := range []struct {
			name, want string
			done       chan result
		}{
			{"stdout", "marker", outDone}, {"stderr", "diagnostic", errDone},
		} {
			select {
			case got := <-stream.done:
				if got.err != nil || string(got.data) != stream.want {
					t.Fatalf("%s=%q err=%v", stream.name, got.data, got.err)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("%s EOF withheld while backend still reads stdin", stream.name)
			}
		}
		// Receiving output EOF must leave the client-to-backend half open.
		if _, err := io.WriteString(stdin, "after-eof"); err != nil {
			t.Fatal(err)
		}
		if err := stdin.Close(); err != nil {
			t.Fatal(err)
		}
		waited := make(chan error, 1)
		go func() { waited <- session.Wait() }()
		select {
		case err := <-waited:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("command did not exit after input EOF")
		}
		check, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		defer check.Close()
		data, err := check.Output("cat /home/workspace/eof-input")
		if err != nil || string(data) != "after-eof" {
			t.Fatalf("input after output EOF=%q err=%v", data, err)
		}
	})
	t.Run("image command path", func(t *testing.T) {
		session, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		data, err := session.Output(`printf '%s' "$PATH"`)
		if err != nil || !strings.HasPrefix(string(data), "/test-bin:") {
			t.Fatalf("image PATH lost: %q %v", data, err)
		}
	})
	t.Run("PTY shell resize and signal", func(t *testing.T) {
		session, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		if err := session.RequestPty("xterm", 24, 80, ssh.TerminalModes{ssh.ECHO: 0}); err != nil {
			t.Fatal(err)
		}
		stdin, err := session.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := session.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Shell(); err != nil {
			t.Fatal(err)
		}
		if err := session.WindowChange(41, 99); err != nil {
			t.Fatal(err)
		}
		io.WriteString(stdin, "stty size; printf 'TERM=%s HOME=%s\\n' \"$TERM\" \"$HOME\"; exit\n")
		output, err := io.ReadAll(stdout)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Wait(); err != nil {
			t.Fatalf("shell: %v output=%s", err, output)
		}
		if !bytes.Contains(output, []byte("41 99")) || !bytes.Contains(output, []byte("TERM=xterm HOME=/home/workspace")) {
			t.Fatalf("shell output=%s", output)
		}
		killed, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		defer killed.Close()
		if err := killed.Start("sleep 120"); err != nil {
			t.Fatal(err)
		}
		if err := killed.Signal(ssh.SIGTERM); err != nil {
			t.Fatal(err)
		}
		if err := killed.Wait(); err == nil {
			t.Fatal("signal did not terminate process")
		}
	})
	t.Run("SFTP and SCP", func(t *testing.T) {
		keyPath := clientKeyPath
		known := clientKnownHosts
		// OpenSSH's nonstandard-port host form is [host]:port.
		port := ln.Addr().(*net.TCPAddr).Port
		payload := bytes.Repeat([]byte{0, 1, 2, 255}, 256*1024)
		source, destination := filepath.Join(dir, "source"), filepath.Join(dir, "download")
		os.WriteFile(source, payload, 0600)
		opts := []string{"-i", keyPath, "-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + known}
		args := append([]string{"-b", "-", "-P", strconv.Itoa(port)}, opts...)
		args = append(args, "dev@127.0.0.1")
		command := exec.CommandContext(ctx, "sftp", args...)
		command.Stdin = strings.NewReader(fmt.Sprintf("put %s /home/workspace/payload\nget /home/workspace/payload %s\n", source, destination))
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("sftp: %v: %s", err, out)
		}
		data, err := os.ReadFile(destination)
		if err != nil || !bytes.Equal(data, payload) {
			t.Fatalf("SFTP bytes differ: %v", err)
		}
		args = append([]string{"-O", "-P", strconv.Itoa(port)}, opts...)
		args = append(args, "dev@127.0.0.1:/home/workspace/payload", destination)
		if out, err := exec.CommandContext(ctx, "scp", args...).CombinedOutput(); err != nil {
			t.Fatalf("scp: %v: %s", err, out)
		}
		data, err = os.ReadFile(destination)
		if err != nil || !bytes.Equal(data, payload) {
			t.Fatalf("SCP bytes differ: %v", err)
		}
	})
	t.Run("concurrent channels", func(t *testing.T) {
		var wg sync.WaitGroup
		start := make(chan struct{})
		for range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				session, err := client.NewSession()
				if err != nil {
					t.Error(err)
					return
				}
				defer session.Close()
				data, err := session.Output("printf worker")
				if err != nil || string(data) != "worker" {
					t.Errorf("concurrent output=%q err=%v", data, err)
				}
			}()
		}
		close(start)
		wg.Wait()
	})
	for _, transport := range []string{"Go client", "direct OpenSSH CLI", "proxied OpenSSH CLI"} {
		name := "forward output EOF preserves input " + transport
		t.Run(name, func(t *testing.T) {
			docker(t, "exec", container, "python3", "-c", `import pathlib
for name in ["half-close-ready","half-close-input"]:
 pathlib.Path("/home/workspace/"+name).unlink(missing_ok=True)`)
			docker(t, "exec", "-d", container, "python3", "-c", `import pathlib,socket
s=socket.socket()
s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
s.bind(("127.0.0.1",8081))
s.listen(1)
pathlib.Path("/home/workspace/half-close-ready").touch()
c,_=s.accept()
c.sendall(b"greeting")
c.shutdown(socket.SHUT_WR)
data=bytearray()
while True:
 chunk=c.recv(65536)
 if not chunk: break
 data.extend(chunk)
pathlib.Path("/home/workspace/half-close-input.tmp").write_bytes(data)
pathlib.Path("/home/workspace/half-close-input.tmp").rename("/home/workspace/half-close-input")
c.close()
s.close()`)
			docker(t, "exec", container, "python3", "-c", `import pathlib,time
for _ in range(100):
 if pathlib.Path("/home/workspace/half-close-ready").exists(): break
 time.sleep(.05)
else: raise RuntimeError("half-close service not ready")`)
			var conn net.Conn
			if transport != "Go client" {
				port := ln.Addr().(*net.TCPAddr).Port
				keyPath := clientKeyPath
				knownPath := clientKnownHosts
				user := "dev"
				if transport == "direct OpenSSH CLI" {
					port = 2222
					user = "voidshell"
					credentials := t.TempDir()
					keyPath = filepath.Join(credentials, "client")
					knownPath = filepath.Join(credentials, "known_hosts")
					if err := os.WriteFile(keyPath, secret.Data["client-key"], 0600); err != nil {
						t.Fatal(err)
					}
					host, err := ssh.ParsePrivateKey(secret.Data["host-key"])
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(knownPath, []byte(fmt.Sprintf("[127.0.0.1]:%d %s", port, ssh.MarshalAuthorizedKey(host.PublicKey()))), 0600); err != nil {
						t.Fatal(err)
					}
				}
				reservation, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				localPort := reservation.Addr().(*net.TCPAddr).Port
				reservation.Close()
				command := exec.CommandContext(ctx, "ssh", "-F", "/dev/null", "-p", strconv.Itoa(port), "-i", keyPath, "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile="+knownPath, "-o", "ExitOnForwardFailure=yes", "-N", "-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:8081", localPort), user+"@127.0.0.1")
				var diagnostics bytes.Buffer
				command.Stderr = &diagnostics
				if err := command.Start(); err != nil {
					t.Fatal(err)
				}
				defer func() { command.Process.Kill(); command.Wait() }()
				for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
					conn, err = net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", localPort), 100*time.Millisecond)
					if err == nil {
						break
					}
					time.Sleep(20 * time.Millisecond)
				}
				if err != nil {
					t.Fatalf("SSH -L listener: %v", err)
				}
			} else {
				var err error
				conn, err = client.Dial("tcp", "127.0.0.1:8081")
				if err != nil {
					t.Fatal(err)
				}
			}
			defer conn.Close()
			half, ok := conn.(interface{ CloseWrite() error })
			if !ok {
				t.Fatal("forward has no input half-close")
			}

			type result struct {
				data []byte
				err  error
			}
			output := make(chan result, 1)
			go func() { data, err := io.ReadAll(conn); output <- result{data, err} }()
			select {
			case got := <-output:
				if got.err != nil || string(got.data) != "greeting" {
					t.Fatalf("forwarded output=%q err=%v", got.data, got.err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("forwarded output EOF withheld")
			}
			payload := "input after output EOF"
			if n, err := io.WriteString(conn, payload); err != nil || n != len(payload) {
				t.Fatalf("input after output EOF: wrote %d bytes, err=%v", n, err)
			}
			if err := half.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			if transport != "Go client" {
				conn.Close()
			}
			docker(t, "exec", container, "python3", "-c", `import pathlib,time
p=pathlib.Path("/home/workspace/half-close-input")
for _ in range(100):
 if p.exists(): break
 time.sleep(.05)
else: raise RuntimeError("forwarded input never reached EOF")
assert p.read_bytes()==b"input after output EOF",p.read_bytes()`)
		})
	}
	t.Run("loopback forwarding", func(t *testing.T) {
		docker(t, "exec", "-d", container, "python3", "-m", "http.server", "8080", "--bind", "127.0.0.1")
		docker(t, "exec", container, "python3", "-c", `import socket,time
for attempt in range(100):
 try:
  socket.create_connection(("127.0.0.1",8080),timeout=.1).close()
  break
 except OSError: time.sleep(.05)
else: raise RuntimeError("HTTP server not ready")`)
		var conn net.Conn
		var err error
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			conn, err = client.Dial("tcp", "127.0.0.1:8080")
			if err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		fmt.Fprint(conn, "GET / HTTP/1.0\r\nHost: localhost\r\n\r\n")
		data, err := io.ReadAll(conn)
		if err != nil || !bytes.Contains(data, []byte("200 OK")) {
			t.Fatalf("forward: %v %s", err, data)
		}
		if c, err := client.Dial("tcp", "10.0.0.1:80"); err == nil {
			c.Close()
			t.Fatal("arbitrary cluster forwarding accepted")
		}
	})
	// The SSH backend's tests use only fake lifecycle API calls. Any fallback to
	// pods/exec or pods/portforward would fail because restCfg is deliberately nil.
}
