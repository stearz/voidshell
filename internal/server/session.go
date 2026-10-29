package server

import (
	"context"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/ssh"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/stearz/voidshell/internal/workspace"
)

// Wire format structs for SSH channel requests (RFC 4254).
type ptyRequestMsg struct {
	Term     string
	Columns  uint32
	Rows     uint32
	Width    uint32
	Height   uint32
	Modelist string
}

type windowChangeMsg struct {
	Columns uint32
	Rows    uint32
	Width   uint32
	Height  uint32
}

type exitStatusMsg struct {
	Status uint32
}

// terminalSizeQueue implements remotecommand.TerminalSizeQueue via a buffered channel.
type terminalSizeQueue struct {
	ch chan remotecommand.TerminalSize
}

func newTerminalSizeQueue() *terminalSizeQueue {
	return &terminalSizeQueue{ch: make(chan remotecommand.TerminalSize, 16)}
}

func (q *terminalSizeQueue) Next() *remotecommand.TerminalSize {
	size, ok := <-q.ch
	if !ok {
		return nil
	}
	return &size
}

func (q *terminalSizeQueue) push(cols, rows uint32) {
	select {
	case q.ch <- remotecommand.TerminalSize{Width: uint16(cols), Height: uint16(rows)}:
	default: // drop resize event when queue is full
	}
}

func (q *terminalSizeQueue) stop() {
	close(q.ch)
}

func (s *Server) handleSession(ctx context.Context, newChan ssh.NewChannel, id workspace.Identity, mode workspace.StorageMode) {
	ch, reqs, err := newChan.Accept()
	if err != nil {
		return
	}
	defer ch.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var tty bool
	var term string
	cols, rows := uint32(80), uint32(24)
	sizes := newTerminalSizeQueue()
	defer sizes.stop()
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			var msg ptyRequestMsg
			if tty || ssh.Unmarshal(req.Payload, &msg) != nil || strings.ContainsRune(msg.Term, 0) {
				req.Reply(false, nil)
				continue
			}
			tty, term = true, msg.Term
			if msg.Columns > 0 && msg.Rows > 0 {
				cols, rows = msg.Columns, msg.Rows
			}
			req.Reply(true, nil)
		case "shell", "exec", "subsystem":
			var command []string
			valid := true
			switch req.Type {
			case "shell":
				valid = len(req.Payload) == 0
			case "exec":
				var msg struct{ Command string }
				valid = ssh.Unmarshal(req.Payload, &msg) == nil && !strings.ContainsRune(msg.Command, 0)
				command = []string{"/bin/sh", "-c", msg.Command}
			case "subsystem":
				var msg struct{ Name string }
				valid = ssh.Unmarshal(req.Payload, &msg) == nil && msg.Name == "sftp" && !tty
				// exec replaces the wrapper: stdout must contain only SFTP protocol data.
				command = []string{"/bin/sh", "-c", `for p in /usr/lib/openssh/sftp-server /usr/libexec/openssh/sftp-server /usr/libexec/sftp-server; do if [ -x "$p" ]; then exec "$p"; fi; done; echo 'voidshell: sftp-server not installed' >&2; exit 127`}
			}
			if !valid {
				req.Reply(false, nil)
				continue
			}
			req.Reply(true, nil)
			sizes.push(cols, rows)
			// Join the request consumer before closing the resize queue.
			drained := make(chan struct{})
			go func() {
				defer close(drained)
				defer cancel()
				drainRequests(reqs, sizes)
			}()
			s.runSession(ctx, ch, id, mode, command, tty, term, sizes)
			ch.Close()
			<-drained
			return
		default:
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}
}

func drainRequests(reqs <-chan *ssh.Request, sizes *terminalSizeQueue) {
	for req := range reqs {
		if req.Type == "window-change" {
			var msg windowChangeMsg
			if ssh.Unmarshal(req.Payload, &msg) == nil {
				sizes.push(msg.Columns, msg.Rows)
			}
		}
		if req.WantReply {
			req.Reply(false, nil)
		}
	}
}

func (s *Server) runSession(ctx context.Context, ch ssh.Channel, id workspace.Identity, mode workspace.StorageMode, command []string, tty bool, term string, sizes *terminalSizeQueue) {
	code := uint32(1)
	// Cleanup finishes before exit-status is sent, so an immediate reconnect
	// cannot race a pod that is still being deleted.
	defer func() { sendExitStatus(ch, code) }()
	release, err := s.acquireWorkspace(ctx, id, mode)
	if err != nil {
		fmt.Fprintf(ch.Stderr(), "voidshell: workspace setup failed: %v\n", err)
		return
	}
	defer release()
	var stderr io.Writer
	if !tty {
		stderr = ch.Stderr()
	}
	if command == nil {
		code, err = s.attacher.Attach(ctx, id, mode, term, tty, sizes, ch, ch, stderr)
	} else {
		if tty {
			command = append([]string{"env", "TERM=" + term}, command...)
		}
		code, err = s.attacher.Exec(ctx, id, mode, command, tty, sizes, ch, ch, stderr)
	}
	if err != nil {
		code = 1
		s.log.Error("session: execution failed", "workspace", id.WorkspaceID(), "error", err)
		fmt.Fprintf(ch.Stderr(), "voidshell: execution failed: %v\n", err)
	}
}

func sendExitStatus(ch ssh.Channel, code uint32) {
	ch.SendRequest("exit-status", false, ssh.Marshal(exitStatusMsg{code}))
}
