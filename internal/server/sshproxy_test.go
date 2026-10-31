package server

import (
	"golang.org/x/crypto/ssh"
	"testing"
)

func TestProxyRequestBoundary(t *testing.T) {
	for _, tc := range []struct {
		name               string
		req                ssh.Request
		started, pty, want bool
	}{
		{name: "shell", req: ssh.Request{Type: "shell"}, want: true},
		{name: "second shell", req: ssh.Request{Type: "shell"}, started: true},
		{name: "shell payload", req: ssh.Request{Type: "shell", Payload: []byte{0}}},
		{name: "exec", req: ssh.Request{Type: "exec", Payload: ssh.Marshal(struct{ Command string }{"echo 'hello world'"})}, want: true},
		{name: "exec nul", req: ssh.Request{Type: "exec", Payload: ssh.Marshal(struct{ Command string }{"echo\x00"})}},
		{name: "sftp", req: ssh.Request{Type: "subsystem", Payload: ssh.Marshal(struct{ Name string }{"sftp"})}, want: true},
		{name: "unknown subsystem", req: ssh.Request{Type: "subsystem", Payload: ssh.Marshal(struct{ Name string }{"other"})}},
		{name: "sftp pty", req: ssh.Request{Type: "subsystem", Payload: ssh.Marshal(struct{ Name string }{"sftp"})}, pty: true},
		{name: "pty", req: ssh.Request{Type: "pty-req", Payload: ssh.Marshal(ptyRequestMsg{Term: "xterm", Rows: 24, Columns: 80})}, want: true},
		{name: "late pty", req: ssh.Request{Type: "pty-req", Payload: ssh.Marshal(ptyRequestMsg{Term: "xterm"})}, started: true},
		{name: "resize", req: ssh.Request{Type: "window-change", Payload: ssh.Marshal(windowChangeMsg{Rows: 30, Columns: 90})}, started: true, pty: true, want: true},
		{name: "malformed resize", req: ssh.Request{Type: "window-change", Payload: []byte{0}}, pty: true},
		{name: "signal", req: ssh.Request{Type: "signal", Payload: ssh.Marshal(struct{ Signal string }{"TERM"})}, started: true, want: true},
		{name: "early signal", req: ssh.Request{Type: "signal", Payload: ssh.Marshal(struct{ Signal string }{"TERM"})}},
		{name: "environment", req: ssh.Request{Type: "env", Payload: ssh.Marshal(struct{ Name, Value string }{"LD_PRELOAD", "/tmp/hook"})}},
		{name: "agent", req: ssh.Request{Type: "auth-agent-req@openssh.com"}},
		{name: "X11", req: ssh.Request{Type: "x11-req"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validProxyRequest(&tc.req, tc.started, tc.pty); got != tc.want {
				t.Fatalf("accepted=%v want=%v", got, tc.want)
			}
		})
	}
}
