package k8s

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"

	"github.com/stearz/voidshell/internal/workspace"
)

// Forward uses the Kubernetes API to reach the selected pod's loopback, including
// services that do not bind the pod IP. It never connects to arbitrary hosts.
func (m *Manager) Forward(ctx context.Context, id workspace.Identity, mode workspace.StorageMode, port uint16, stdin io.Reader, stdout io.Writer) error {
	if m.restCfg == nil {
		return fmt.Errorf("port forwarding unavailable: Manager has no REST config")
	}
	transport, upgrader, err := spdy.RoundTripperFor(m.restCfg)
	if err != nil {
		return err
	}
	url := m.client.CoreV1().RESTClient().Post().Resource("pods").Name(id.PodNameFor(mode)).Namespace(m.cfg.Namespace).SubResource("portforward").URL()
	req, err := http.NewRequestWithContext(ctx, "POST", url.String(), nil)
	if err != nil {
		return err
	}
	conn, protocol, err := spdy.Negotiate(upgrader, &http.Client{Transport: transport}, req, portforward.PortForwardProtocolV1Name)
	if err != nil {
		return err
	}
	defer conn.Close()
	if protocol != portforward.PortForwardProtocolV1Name {
		return fmt.Errorf("unsupported port-forward protocol %q", protocol)
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	headers := http.Header{}
	headers.Set(corev1.PortHeader, strconv.Itoa(int(port)))
	headers.Set(corev1.PortForwardRequestIDHeader, "0")
	headers.Set(corev1.StreamType, corev1.StreamTypeError)
	errors, err := conn.CreateStream(headers)
	if err != nil {
		return err
	}
	errors.Close() // Close the write side; the read side carries kubelet errors.
	errorResult := make(chan error, 1)
	go func() {
		message, err := io.ReadAll(io.LimitReader(errors, 16384))
		if len(message) > 0 {
			err = fmt.Errorf("pod port-forward: %s", message)
		}
		errorResult <- err
	}()
	headers.Set(corev1.StreamType, corev1.StreamTypeData)
	data, err := conn.CreateStream(headers)
	if err != nil {
		return err
	}
	localError := make(chan error, 1)
	remoteDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(data, stdin)
		data.Close() // Preserve TCP half-close: the remote side may still reply.
		if err != nil {
			localError <- err
		}
	}()
	go func() { _, err := io.Copy(stdout, data); remoteDone <- err }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-localError:
		return err
	case err := <-remoteDone:
		data.Reset()
		select {
		case remoteErr := <-errorResult:
			if remoteErr != nil {
				return remoteErr
			}
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	case err := <-errorResult:
		if err != nil {
			return err
		}
		// Normal remote completion: drain the data before closing the SSH channel.
		select {
		case err := <-remoteDone:
			return err
		case err := <-localError:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
