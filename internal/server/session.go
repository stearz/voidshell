package server

import (
	"context"
	"fmt"
	"io"
	"time"

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

func (s *Server) handleSession(ctx context.Context, newChan ssh.NewChannel, id workspace.Identity) {
	ch, reqs, err := newChan.Accept()
	if err != nil {
		s.log.Error("session: channel accept failed", "error", err)
		return
	}
	defer ch.Close()

	var (
		ptyRequested bool
		initCols     uint32 = 80
		initRows     uint32 = 24
	)
	sizeQueue := newTerminalSizeQueue()

	for req := range reqs {
		switch req.Type {
		case "pty-req":
			var msg ptyRequestMsg
			if err := ssh.Unmarshal(req.Payload, &msg); err == nil && msg.Columns > 0 {
				initCols = msg.Columns
				initRows = msg.Rows
			}
			ptyRequested = true
			req.Reply(true, nil)

		case "shell":
			req.Reply(true, nil)
			sizeQueue.push(initCols, initRows)
			go drainRequests(reqs, sizeQueue)
			s.runShellSession(ctx, ch, id, ptyRequested, sizeQueue)
			return

		case "exec":
			req.Reply(false, nil)
			sizeQueue.stop()
			return

		default:
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}
	// reqs closed without receiving a shell request
	sizeQueue.stop()
}

// drainRequests consumes the SSH request channel, forwarding window-change
// events to sizeQueue, until the channel is closed.
func drainRequests(reqs <-chan *ssh.Request, sizeQueue *terminalSizeQueue) {
	defer sizeQueue.stop()
	for req := range reqs {
		if req.Type == "window-change" {
			var msg windowChangeMsg
			if err := ssh.Unmarshal(req.Payload, &msg); err == nil {
				sizeQueue.push(msg.Columns, msg.Rows)
			}
		}
		if req.WantReply {
			req.Reply(false, nil)
		}
	}
}

func (s *Server) runShellSession(ctx context.Context, ch ssh.Channel, id workspace.Identity, tty bool, sizeQueue *terminalSizeQueue) {
	if err := s.lifecycle.EnsureWorkspace(ctx, id); err != nil {
		s.log.Error("session: workspace setup failed",
			"error", err,
			"workspace", id.WorkspaceID(),
			"github_user", id.GithubUser,
			"ssh_user", id.SSHUser,
		)
		fmt.Fprintf(ch.Stderr(), "voidshell: workspace setup failed: %v\n", err)
		sendExitStatus(ch, 1)
		return
	}

	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.lifecycle.DeletePod(cleanupCtx, id); err != nil {
			s.log.Error("session: pod cleanup failed",
				"error", err,
				"workspace", id.WorkspaceID(),
			)
		}
	}()

	// When using a PTY the container runtime multiplexes stderr into stdout.
	var stderr io.Writer
	if !tty {
		stderr = ch.Stderr()
	}

	if err := s.attacher.Attach(ctx, id, tty, sizeQueue, ch, ch, stderr); err != nil {
		s.log.Info("session: attach ended with error",
			"error", err,
			"workspace", id.WorkspaceID(),
		)
		sendExitStatus(ch, 1)
		return
	}
	sendExitStatus(ch, 0)
}

func sendExitStatus(ch ssh.Channel, code uint32) {
	ch.SendRequest("exit-status", false, ssh.Marshal(exitStatusMsg{code}))
}
