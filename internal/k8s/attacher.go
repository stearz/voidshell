package k8s

import (
	"context"
	"errors"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"

	"github.com/stearz/voidshell/internal/workspace"
)

// Attach starts an independent shell process; it never attaches to pod PID 1.
func (m *Manager) Attach(ctx context.Context, id workspace.Identity, mode workspace.StorageMode, term string, tty bool, sizes remotecommand.TerminalSizeQueue, stdin io.Reader, stdout, stderr io.Writer) (uint32, error) {
	command := m.cfg.ShellCommand
	if len(command) == 0 {
		command = []string{"/bin/bash", "-l"}
	}
	if tty {
		command = append([]string{"env", "TERM=" + term}, command...)
	}
	return m.Exec(ctx, id, mode, command, tty, sizes, stdin, stdout, stderr)
}

// Exec preserves remote exit codes. A nonzero process exit is not an API error.
func (m *Manager) Exec(ctx context.Context, id workspace.Identity, mode workspace.StorageMode, command []string, tty bool, sizes remotecommand.TerminalSizeQueue, stdin io.Reader, stdout, stderr io.Writer) (uint32, error) {
	if m.restCfg == nil {
		return 1, fmt.Errorf("exec not available: Manager has no REST config")
	}
	req := m.client.CoreV1().RESTClient().Post().Resource("pods").Name(id.PodNameFor(mode)).Namespace(m.cfg.Namespace).SubResource("exec").VersionedParams(&corev1.PodExecOptions{
		Container: "workspace", Command: command, Stdin: stdin != nil, Stdout: stdout != nil, Stderr: !tty && stderr != nil, TTY: tty,
	}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(m.restCfg, "POST", req.URL())
	if err != nil {
		return 1, fmt.Errorf("creating exec stream: %w", err)
	}
	if tty {
		stderr = nil
	}
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: stdin, Stdout: stdout, Stderr: stderr, Tty: tty, TerminalSizeQueue: sizes})
	return executionResult(err)
}

func executionResult(err error) (uint32, error) {
	if err == nil {
		return 0, nil
	}
	var exit utilexec.ExitError
	if errors.As(err, &exit) && exit.Exited() {
		return uint32(exit.ExitStatus()), nil
	}
	return 1, err
}
