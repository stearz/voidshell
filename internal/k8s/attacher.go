package k8s

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"k8s.io/client-go/tools/remotecommand"

	"github.com/stearz/voidshell/internal/workspace"
)

// Attach streams stdin/stdout/stderr between the caller and the workspace
// container's running process using the Kubernetes pod attach API.
//
// When tty is true the container runtime multiplexes stderr into stdout, so
// stderr is ignored. Pass a nil stderr in that case or any writer — it will
// not receive data. resizeQueue may be nil when tty is false.
func (m *Manager) Attach(
	ctx context.Context,
	id workspace.Identity,
	tty bool,
	resizeQueue remotecommand.TerminalSizeQueue,
	stdin io.Reader,
	stdout, stderr io.Writer,
) error {
	if m.restCfg == nil {
		return fmt.Errorf("attach not available: Manager has no REST config (use NewFromKubeconfig, not New)")
	}

	req := m.client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(id.PodName()).
		Namespace(m.cfg.Namespace).
		SubResource("attach").
		Param("container", "workspace").
		Param("stdin", "true").
		Param("stdout", "true").
		Param("stderr", strconv.FormatBool(!tty)).
		Param("tty", strconv.FormatBool(tty))

	exec, err := remotecommand.NewSPDYExecutor(m.restCfg, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("creating SPDY executor: %w", err)
	}

	return exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:             stdin,
		Stdout:            stdout,
		Stderr:            stderr,
		Tty:               tty,
		TerminalSizeQueue: resizeQueue,
	})
}
