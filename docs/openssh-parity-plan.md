# OpenSSH Parity Implementation Plan

## Goal

Make voidshell's SSH server behave like a full OpenSSH server from a client's perspective.
Concretely: `scp`, `sftp`, `rsync`, `ssh -L`, `ssh -R`, and non-interactive `ssh user@host cmd` must all work.

## Current state

| Feature | Status | Notes |
|---------|--------|-------|
| Interactive shell (`ssh user@host`) | ✅ | |
| PTY + window resize | ✅ | |
| `exec` (`ssh user@host cmd`) | ❌ | Hard-rejected in `session.go:97` |
| `scp` | ❌ | Depends on exec |
| `rsync` | ❌ | Depends on exec |
| SFTP (`sftp`, `scp -s`) | ❌ | No subsystem handler |
| Local port-forward (`-L`) | ❌ | `direct-tcpip` channels rejected |
| Remote port-forward (`-R`) | ❌ | `tcpip-forward` global request discarded |
| Environment variables (`env` request) | ❌ | |
| Signal forwarding | ❌ | |
| Keepalive global requests | ⚠️ | Discarded silently; fine but can cause hangs |

## Kubernetes constraints

This is not a conventional SSH server — it is a proxy to Kubernetes pods. Every feature
must be mapped to a k8s API call:

- **exec/shell**: `pods/attach` (existing) or `pods/exec` (new)
- **port forwarding into pod**: k8s pod IP + direct TCP from in-cluster server, or `pods/portforward` SPDY
- **SFTP**: k8s exec of `sftp-server` binary inside the workspace pod
- **env vars**: cannot be set on a running container; must be prepended to exec commands as `env K=V cmd`
- **signals**: not feasible without exec-ing `kill` inside the pod — deferred (see Phase 4)

---

## Phase 1 — `exec` (enables `scp`, `rsync`, scripted SSH)

### What changes

**`internal/server/server.go`** — extend `PodAttacher` interface:

```go
type PodAttacher interface {
    Attach(ctx context.Context, id workspace.Identity, tty bool, resizeQueue remotecommand.TerminalSizeQueue, stdin io.Reader, stdout, stderr io.Writer) error
    Exec(ctx context.Context, id workspace.Identity, command []string, tty bool, resizeQueue remotecommand.TerminalSizeQueue, stdin io.Reader, stdout, stderr io.Writer) (uint32, error)
}
```

`Exec` returns `(exitCode, error)` so the session can send a correct `exit-status` to the client.
`error` is non-nil only for infrastructure failures (can't reach k8s); a non-zero exit code is
not an error — it is normal program termination.

**`internal/k8s/attacher.go`** — add `Exec` method:

```go
func (m *Manager) Exec(ctx context.Context, id workspace.Identity, command []string,
    tty bool, resizeQueue remotecommand.TerminalSizeQueue,
    stdin io.Reader, stdout, stderr io.Writer) (uint32, error) {

    req := m.client.CoreV1().RESTClient().Post().
        Resource("pods").Name(id.PodName()).
        Namespace(m.cfg.Namespace).SubResource("exec").
        Param("container", "workspace").
        Param("stdin", "true").
        Param("stdout", "true").
        Param("stderr", strconv.FormatBool(!tty)).
        Param("tty", strconv.FormatBool(tty))
    for _, arg := range command {
        req = req.Param("command", arg)
    }

    executor, err := remotecommand.NewSPDYExecutor(m.restCfg, "POST", req.URL())
    if err != nil {
        return 1, fmt.Errorf("creating SPDY executor: %w", err)
    }

    err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
        Stdin: stdin, Stdout: stdout, Stderr: stderr,
        Tty: tty, TerminalSizeQueue: resizeQueue,
    })
    return extractExitCode(err), filterInfraError(err)
}
```

`extractExitCode` parses the `k8s.io/apimachinery/pkg/api/errors.StatusError` that
`remotecommand` returns when the remote process exits non-zero. The status has
`Reason == "ExitError"` and `Details.Causes[0].Message` is the decimal exit code.

```go
func extractExitCode(err error) uint32 {
    if err == nil {
        return 0
    }
    var statusErr *apierrors.StatusError
    if errors.As(err, &statusErr) && statusErr.Status().Reason == metav1.StatusReasonExitError {
        for _, c := range statusErr.Status().Details.Causes {
            if c.Type == "ExitCode" {
                if code, e := strconv.Atoi(c.Message); e == nil {
                    return uint32(code)
                }
            }
        }
    }
    return 1
}
```

**`internal/server/session.go`** — replace the exec rejection with a real handler:

Wire format (RFC 4254 §6.5):
```
string  command   // shell-quoted command line
```

```go
case "exec":
    var msg struct{ Command string }
    if err := ssh.Unmarshal(req.Payload, &msg); err != nil {
        req.Reply(false, nil)
        return
    }
    req.Reply(true, nil)
    sizeQueue.push(initCols, initRows)
    go drainRequests(reqs, sizeQueue)
    s.runExecSession(ctx, ch, id, msg.Command, ptyRequested, sizeQueue)
    return
```

```go
func (s *Server) runExecSession(ctx context.Context, ch ssh.Channel, id workspace.Identity,
    command string, tty bool, sizeQueue *terminalSizeQueue) {

    if err := s.lifecycle.EnsureWorkspace(ctx, id); err != nil { ... }
    defer s.lifecycle.DeletePod(...)

    // Shell-split the command so k8s exec receives it as argv, not a single string.
    // Use []string{"sh", "-c", command} to honour shell quoting and pipelines.
    argv := []string{"sh", "-c", command}

    var stderr io.Writer
    if !tty {
        stderr = ch.Stderr()
    }
    exitCode, err := s.attacher.Exec(ctx, id, argv, tty, sizeQueue, ch, ch, stderr)
    if err != nil {
        s.log.Error("exec: infrastructure error", "error", err)
        sendExitStatus(ch, 1)
        return
    }
    sendExitStatus(ch, exitCode)
}
```

### New RBAC requirement

Add `pods/exec: [create]` to the voidshell ServiceAccount RBAC (documented in `k8s/lifecycle.go` header comment).

### Testing

- Unit: mock `PodAttacher.Exec`, verify exit code is forwarded correctly
- Integration checklist: `ssh user@host ls /home/workspace`, `scp file user@host:/home/workspace/`, `rsync -avz src/ user@host:/home/workspace/`

---

## Phase 2 — Port forwarding (`-L` and `-R`)

### 2a — Local port forwarding (`-L localport:host:port`)

When the client does `ssh -L 8080:localhost:3000 user@host`, it opens a `direct-tcpip`
channel for each TCP connection to `localhost:8080`. The server must proxy the data to
`host:3000`.

**Strategy**: since voidshell runs in-cluster, it can reach the workspace pod's cluster IP
via `net.Dial`. We get the pod IP from the k8s API and dial the requested port directly.
`host` in the channel payload is resolved from the pod's perspective; `localhost` means the pod.

Wire format (RFC 4254 §7.2):
```
string  host to connect
uint32  port to connect
string  originator IP address
uint32  originator port
```

**`internal/server/server.go`** — new interface and `handleConn` changes:

```go
type PodDialer interface {
    DialPod(ctx context.Context, id workspace.Identity, host string, port uint32) (net.Conn, error)
}
```

Add `dialer PodDialer` to `Server`. In `handleConn`, replace `go ssh.DiscardRequests(reqs)` with a
goroutine that handles global requests (needed for Phase 2b). Accept `direct-tcpip` channels:

```go
for newChan := range chans {
    switch newChan.ChannelType() {
    case "session":
        go s.handleSession(ctx, newChan, id, sshConn)
    case "direct-tcpip":
        go s.handleDirectTCPIP(ctx, newChan, id)
    default:
        newChan.Reject(ssh.UnknownChannelType, "unsupported channel type")
    }
}
```

**`internal/server/portfwd.go`** (new file):

```go
func (s *Server) handleDirectTCPIP(ctx context.Context, newChan ssh.NewChannel, id workspace.Identity) {
    var msg directTCPIPMsg // host, port, origIP, origPort
    if err := ssh.Unmarshal(newChan.ExtraData(), &msg); err != nil {
        newChan.Reject(ssh.Prohibited, "bad payload")
        return
    }
    conn, err := s.dialer.DialPod(ctx, id, msg.DestHost, msg.DestPort)
    if err != nil {
        newChan.Reject(ssh.ConnectionFailed, err.Error())
        return
    }
    defer conn.Close()
    ch, reqs, _ := newChan.Accept()
    defer ch.Close()
    go ssh.DiscardRequests(reqs)
    proxy(ch, conn) // bidirectional io.Copy
}
```

**`internal/k8s/portfwd.go`** (new file):

```go
func (m *Manager) DialPod(ctx context.Context, id workspace.Identity, host string, port uint32) (net.Conn, error) {
    pod, err := m.client.CoreV1().Pods(m.cfg.Namespace).Get(ctx, id.PodName(), metav1.GetOptions{})
    if err != nil {
        return nil, fmt.Errorf("getting pod IP: %w", err)
    }
    if pod.Status.PodIP == "" {
        return nil, fmt.Errorf("pod %q has no IP yet", id.PodName())
    }
    // "localhost" and "127.0.0.1" from the client mean "the pod's loopback"
    target := pod.Status.PodIP
    return (&net.Dialer{}).DialContext(ctx, "tcp", fmt.Sprintf("%s:%d", target, port))
}
```

### 2b — Remote port forwarding (`-R remoteport:host:port`)

The client sends a `tcpip-forward` global request asking the server to listen on a port. When
a connection arrives the server opens a `forwarded-tcpip` channel back to the client.

Wire format — `tcpip-forward` request payload:
```
string  address to bind (e.g. "localhost" or "")
uint32  port to bind
```

Response (when port was 0, server picks one):
```
uint32  bound port
```

Wire format — `forwarded-tcpip` channel extra data:
```
string  address that was connected
uint32  port that was connected
string  originator IP
uint32  originator port
```

**`internal/server/portfwd.go`** — global request handler:

```go
func (s *Server) handleGlobalRequests(ctx context.Context, reqs <-chan *ssh.Request, sshConn *ssh.ServerConn, id workspace.Identity) {
    listeners := map[uint32]net.Listener{} // bound port → listener
    defer func() {
        for _, ln := range listeners { ln.Close() }
    }()

    for req := range reqs {
        switch req.Type {
        case "tcpip-forward":
            ln, boundPort, err := s.startRemoteForward(ctx, req, sshConn, id)
            if err != nil {
                req.Reply(false, nil)
                continue
            }
            listeners[boundPort] = ln
            // reply with bound port if client requested 0
            ...
        case "cancel-tcpip-forward":
            // close and delete from map
        case "keepalive@openssh.com", "no-more-sessions@openssh.com":
            if req.WantReply { req.Reply(true, nil) }
        default:
            if req.WantReply { req.Reply(false, nil) }
        }
    }
}
```

`startRemoteForward` calls `net.Listen("tcp", addr)`, then runs a goroutine that accepts
connections and for each one opens a `forwarded-tcpip` channel on `sshConn` and proxies data.

**Security note**: bind address should be restricted to `localhost`/`127.0.0.1` by default. Binding
to `0.0.0.0` would expose ports on the voidshell server's network interface. Add config opt
`ssh.allowRemoteForwardAnyAddress: false` (default false).

**`internal/server/server.go`** — pass `*ssh.ServerConn` to the goroutine:

```go
// replace: go ssh.DiscardRequests(reqs)
go s.handleGlobalRequests(ctx, reqs, sshConn, id)
```

### New RBAC requirement

`DialPod` reads `pod.Status.PodIP` — requires `pods: [get]`, which is already in the RBAC comment.

### Testing

- `ssh -L 8080:localhost:3000 user@host` then `curl localhost:8080` against a server in the workspace pod
- `ssh -R 9090:localhost:8080 user@host` then expose a local HTTP server through voidshell

---

## Phase 3 — SFTP subsystem

### What changes

The SFTP client (`sftp` command, Cyberduck, VS Code remote-ssh etc.) sends a `subsystem` channel request with name `sftp`. The server runs `sftp-server` inside the workspace pod and pipes stdin/stdout through the channel.

Wire format — `subsystem` request payload:
```
string  subsystem name
```

**`internal/server/session.go`** — add case before `default`:

```go
case "subsystem":
    var msg struct{ Name string }
    if err := ssh.Unmarshal(req.Payload, &msg); err != nil || msg.Name != "sftp" {
        req.Reply(false, nil)
        return
    }
    req.Reply(true, nil)
    go drainRequests(reqs, sizeQueue)
    s.runSubsystem(ctx, ch, id, msg.Name)
    return
```

```go
func (s *Server) runSubsystem(ctx context.Context, ch ssh.Channel, id workspace.Identity, name string) {
    if err := s.lifecycle.EnsureWorkspace(ctx, id); err != nil { ... }
    defer s.lifecycle.DeletePod(...)

    // sftp-server speaks SFTP protocol on stdin/stdout; no TTY needed
    argv := []string{sftpServerPath(name)} // e.g. ["/usr/lib/openssh/sftp-server"]
    exitCode, err := s.attacher.Exec(ctx, id, argv, false, nil, ch, ch, ch.Stderr())
    if err != nil {
        s.log.Error("subsystem: infrastructure error", "name", name, "error", err)
    }
    sendExitStatus(ch, exitCode)
}
```

`sftpServerPath` tries known paths in order: `/usr/lib/openssh/sftp-server`,
`/usr/libexec/sftp-server`. The workspace image should have `openssh-sftp-server` installed.

### Workspace image requirement

Add to `Dockerfile.workspace`:
```dockerfile
RUN apt-get install -y openssh-sftp-server
```

### Testing

- `sftp user@host` interactive session
- `sftp user@host:/home/workspace/file ./local-copy`
- `scp -s` (uses sftp subsystem instead of legacy scp protocol)

---

## Phase 4 — Session refinements

### 4a — `env` request (environment variables)

Wire format (RFC 4254 §6.4):
```
string  variable name
string  variable value
```

The `env` request arrives before `shell` or `exec`. Since we cannot inject env vars into a
running k8s container, we accumulate them in the session state and prepend `env K=V` to exec
commands, or set `TERM` etc. on the pod via the attach call (TERM is already handled via
`ptyRequestMsg.Term`).

**`internal/server/session.go`** — accumulate before shell/exec dispatch:

```go
var envVars []string // collect "K=V" strings

case "env":
    var msg struct{ Name, Value string }
    if err := ssh.Unmarshal(req.Payload, &msg); err == nil {
        if isSafeEnvName(msg.Name) {
            envVars = append(envVars, msg.Name+"="+msg.Value)
        }
    }
    req.Reply(true, nil)
```

For exec, prepend: `argv = append([]string{"env"}, append(envVars, argv...)...)`.
For shell (attach), env vars cannot be forwarded — log and ignore. Most clients only send
`TERM` before shell, which is already extracted from `pty-req`.

`isSafeEnvName` rejects names containing `=`, NUL, or that start with `PATH`/`LD_` to prevent
environment injection attacks.

### 4b — Keepalive / protocol housekeeping

Replace `go ssh.DiscardRequests(reqs)` (already removed in Phase 2b) with the global request
handler that replies `true` to `keepalive@openssh.com` and `no-more-sessions@openssh.com`.
Both arrive frequently from modern OpenSSH clients and failing to reply can cause client hangs.

### 4c — `window-change` in exec sessions

`drainRequests` already forwards `window-change` to `sizeQueue`. This is passed to `Exec`,
so resize works in exec sessions with a PTY (e.g. `ssh user@host vim`). No code change needed —
the existing `drainRequests` goroutine handles this correctly.

### 4d — Signal forwarding (deferred / optional)

Wire format (RFC 4254 §6.9):
```
string  signal name (e.g. "TERM", "INT")
```

Signals cannot be sent to a container process without executing `kill -SIG PID` inside the
pod. This requires knowing the PID, which the k8s exec API does not expose. Implementing this
would require a small wrapper process (PID 1 in the container) that receives signals via stdin
and forwards them. This is out of scope for this plan but noted for future work.

---

## Implementation order and dependencies

```
Phase 1 (exec)
  └── Phase 3 (sftp subsystem)   — subsystem is just exec with a fixed command
Phase 2a (direct-tcpip)          — independent, but needs EnsureWorkspace refactor to expose pod IP
Phase 2b (remote forward)        — needs the global-request handler from 2a
Phase 4a (env)                   — depends on Phase 1 (exec with prepended env)
Phase 4b (keepalive)             — trivial, can be done alongside Phase 2b
```

---

## Files changed summary

| File | Change |
|------|--------|
| `internal/server/server.go` | Extend `PodAttacher` interface; add `PodDialer` interface; thread `sshConn` to goroutines |
| `internal/server/session.go` | Implement exec, env, subsystem request handlers |
| `internal/server/portfwd.go` | New file: `handleDirectTCPIP`, `handleGlobalRequests`, `startRemoteForward` |
| `internal/k8s/attacher.go` | Add `Exec` method with exit code extraction |
| `internal/k8s/portfwd.go` | New file: `DialPod` using pod cluster IP |
| `Dockerfile.workspace` | Add `openssh-sftp-server` package |
| Helm chart RBAC | Add `pods/exec: [create]` |
| `internal/config/config.go` | Add `SSH.AllowRemoteForwardAnyAddress bool` option |

---

## Open questions

1. **Pod lifecycle for exec sessions**: currently `EnsureWorkspace` + `DeletePod` brackets every
   session. For exec, a user running many short commands (`scp file1`, `scp file2`) would create
   and delete the pod each time. Consider keeping the pod alive for a configurable idle TTL and
   only deleting when idle for N minutes.

2. **Port forwarding scope**: should `direct-tcpip` allow forwarding to arbitrary cluster IPs
   (e.g. services), or only to the user's own workspace pod? The current plan restricts to the
   pod IP. A `ssh.allowForwardToCluster` config bool could open this up.

3. **sftp-server path discovery**: hardcoding `/usr/lib/openssh/sftp-server` is fragile.
   Consider making it a `WorkspaceConfig.SFTPServerPath` config field with that as default.
