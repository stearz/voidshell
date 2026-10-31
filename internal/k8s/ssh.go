package k8s

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/stearz/voidshell/internal/workspace"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const workspaceSSHPort = 2222

type sshEndpoint struct {
	address string
	config  *ssh.ClientConfig
}

func (m *Manager) DirectSSHEnabled() bool { return m.cfg.Backend == "ssh" }

func generateSSHKey() ([]byte, ssh.PublicKey, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	block, err := ssh.MarshalPrivateKey(key, "voidshell workspace")
	if err != nil {
		return nil, nil, err
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(block), signer.PublicKey(), nil
}

func (m *Manager) ensureSSHCredentials(ctx context.Context, id workspace.Identity, mode workspace.StorageMode) error {
	pod, err := m.client.CoreV1().Pods(m.cfg.Namespace).Get(ctx, id.PodNameFor(mode), metav1.GetOptions{})
	if err != nil {
		return err
	}
	name := pod.Annotations["voidshell.io/ssh-secret"]
	if name == "" {
		return fmt.Errorf("workspace pod has no SSH credentials reference; delete it before switching backend")
	}
	secrets := m.client.CoreV1().Secrets(m.cfg.Namespace)
	existing, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		if !metav1.IsControlledBy(existing, pod) {
			return fmt.Errorf("SSH secret is not owned by workspace pod")
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	host, _, err := generateSSHKey()
	if err != nil {
		return err
	}
	client, public, err := generateSSHKey()
	if err != nil {
		return err
	}
	immutable := true
	_, err = secrets.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: m.cfg.Namespace,
			Labels:          map[string]string{workspaceLabel: id.WorkspaceID()},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(pod, corev1.SchemeGroupVersion.WithKind("Pod"))}},
		Immutable: &immutable,
		Data:      map[string][]byte{"host-key": host, "client-key": client, "authorized_keys": ssh.MarshalAuthorizedKey(public)},
	}, metav1.CreateOptions{})
	return err
}

func configureSSHPod(pod *corev1.Pod) {
	// Random names prevent a new pod from consuming a terminating pod's credentials.
	name := "workspace-ssh-" + strings.ToLower(rand.Text())
	pod.Annotations = map[string]string{"voidshell.io/ssh-secret": name}
	pod.Labels["voidshell.io/backend"] = "ssh"
	c := &pod.Spec.Containers[0]
	c.Command = []string{"/usr/local/bin/voidshell-sshd"}
	c.Ports = []corev1.ContainerPort{{Name: "workspace-ssh", ContainerPort: workspaceSSHPort}}
	c.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(workspaceSSHPort)}}, PeriodSeconds: 1}
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "ssh", MountPath: "/run/voidshell-ssh", ReadOnly: true})
	permissions := int32(0440)
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: "ssh", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
		SecretName: name, DefaultMode: &permissions,
		// Never expose the proxy's private authentication key to the workspace.
		Items: []corev1.KeyToPath{{Key: "host-key", Path: "host-key"}, {Key: "authorized_keys", Path: "authorized_keys"}},
	}}})
}

// prepareSSH snapshots immutable pod credentials once while provisioning is
// serialized by the workspace lease. Channel creation then needs no API calls.
func (m *Manager) prepareSSH(ctx context.Context, id workspace.Identity, mode workspace.StorageMode) error {
	pod, err := m.client.CoreV1().Pods(m.cfg.Namespace).Get(ctx, id.PodNameFor(mode), metav1.GetOptions{})
	if err != nil {
		return err
	}
	if pod.DeletionTimestamp != nil || net.ParseIP(pod.Status.PodIP) == nil {
		return fmt.Errorf("workspace has no usable pod IP")
	}
	name := pod.Annotations["voidshell.io/ssh-secret"]
	if name == "" {
		return fmt.Errorf("workspace has no SSH secret")
	}
	secret, err := m.client.CoreV1().Secrets(m.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if !metav1.IsControlledBy(secret, pod) {
		return fmt.Errorf("SSH secret is not owned by workspace pod")
	}
	clientKey, err := ssh.ParsePrivateKey(secret.Data["client-key"])
	if err != nil {
		return fmt.Errorf("invalid workspace client key")
	}
	hostKey, err := ssh.ParsePrivateKey(secret.Data["host-key"])
	if err != nil {
		return fmt.Errorf("invalid workspace host key")
	}
	cfg := &ssh.ClientConfig{User: "voidshell", Auth: []ssh.AuthMethod{ssh.PublicKeys(clientKey)}, HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
		if !bytes.Equal(key.Marshal(), hostKey.PublicKey().Marshal()) {
			return fmt.Errorf("workspace SSH host key mismatch")
		}
		return nil
	}}
	address := net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(workspaceSSHPort))
	m.sshEndpoints.Store(id.PodNameFor(mode), &sshEndpoint{address: address, config: cfg})
	return nil
}

// OpenWorkspaceSSH dials only the pod IP prepared for the active lease and pins
// that pod's unique host key. No API requests carry session data or run per channel.
func (m *Manager) OpenWorkspaceSSH(ctx context.Context, id workspace.Identity, mode workspace.StorageMode) (*ssh.Client, error) {
	value, ok := m.sshEndpoints.Load(id.PodNameFor(mode))
	if !ok {
		return nil, fmt.Errorf("workspace SSH endpoint is not prepared")
	}
	endpoint := value.(*sshEndpoint)
	address, cfg := endpoint.address, endpoint.config
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", address)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(dialCtx, func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	transport, chans, reqs, err := ssh.NewClientConn(conn, address, cfg)
	stop()
	if err != nil {
		conn.Close()
		return nil, err
	}
	if err := dialCtx.Err(); err != nil {
		transport.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	return ssh.NewClient(transport, chans, reqs), nil
}

func (m *Manager) forwardSSH(ctx context.Context, id workspace.Identity, mode workspace.StorageMode, port uint16, stdin io.Reader, stdout io.Writer) error {
	client, err := m.OpenWorkspaceSSH(ctx, id, mode)
	if err != nil {
		return err
	}
	defer client.Close()
	stop := context.AfterFunc(ctx, func() { client.Close() })
	defer stop()
	// Keep the backend channel-close notification: DialContext discards it,
	// preventing us from knowing when OpenSSH has flushed the TCP write queue.
	request := struct {
		Host       string
		Port       uint32
		OriginHost string
		OriginPort uint32
	}{"127.0.0.1", uint32(port), "127.0.0.1", 0}
	conn, requests, err := client.OpenChannel("direct-tcpip", ssh.Marshal(request))
	if err != nil {
		return err
	}
	defer conn.Close()
	// Cancellation must interrupt setup immediately. After forwarding starts,
	// normal output EOF followed by client close needs a bounded input drain.
	stop()
	if err := ctx.Err(); err != nil {
		return err
	}
	remoteClosed := make(chan struct{})
	go func() { ssh.DiscardRequests(requests); close(remoteClosed) }()
	go io.Copy(io.Discard, conn.Stderr())
	inputDone, outputDone := make(chan error, 1), make(chan error, 1)
	var outputEOF atomic.Bool
	go func() {
		_, err := io.Copy(conn, stdin)
		conn.CloseWrite()
		inputDone <- err
	}()
	go func() {
		_, err := io.Copy(stdout, conn)
		if err == nil {
			outputEOF.Store(true)
		}
		if half, ok := stdout.(interface{ CloseWrite() error }); ok {
			half.CloseWrite()
		}
		outputDone <- err
	}()
	cancelled := ctx.Done()
	var drainTimer *time.Timer
	var drainExpired <-chan time.Time
	defer func() {
		if drainTimer != nil {
			drainTimer.Stop()
		}
	}()
	// A normal OpenSSH client closes its channel/transport once both EOFs have
	// been exchanged. Its last input bytes may still be buffered locally or in
	// the backend's TCP queue. Preserve them and await the backend's CLOSE before
	// dropping that transport. An abort before output EOF still cancels at once.
	for inputDone != nil || outputDone != nil || remoteClosed != nil {
		select {
		case err := <-inputDone:
			if err != nil {
				return err
			}
			inputDone = nil
		case err := <-outputDone:
			if err != nil {
				return err
			}
			outputDone = nil
		case <-remoteClosed:
			remoteClosed = nil
		case <-cancelled:
			// Mark EOF before sending it, so an immediate peer CLOSE cannot
			// race the output-copy goroutine's completion notification.
			if !outputEOF.Load() {
				return ctx.Err()
			}

			cancelled = nil
			drainTimer = time.NewTimer(5 * time.Second)
			drainExpired = drainTimer.C
		case <-drainExpired:
			return ctx.Err()
		}
	}
	return nil
}
