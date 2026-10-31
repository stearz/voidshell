package k8s

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/stearz/voidshell/internal/workspace"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes/fake"
)

func sshFixture(t *testing.T) (*Manager, workspace.Identity, *corev1.Pod) {
	t.Helper()
	id := workspace.New("alice", "dev")
	pod := buildPod(id, workspace.StorageEphemeral, Config{Namespace: "test", Backend: "ssh"})
	pod.UID = types.UID("test-pod")
	pod.Status.PodIP = "127.0.0.1"
	m := New(fake.NewClientset(pod), Config{Namespace: "test", Backend: "ssh"})
	if err := m.ensureSSHCredentials(context.Background(), id, workspace.StorageEphemeral); err != nil {
		t.Fatal(err)
	}
	if err := m.prepareSSH(context.Background(), id, workspace.StorageEphemeral); err != nil {
		t.Fatal(err)
	}
	return m, id, pod
}

func TestSSHCredentialsArePodScoped(t *testing.T) {
	ctx := context.Background()
	m, id, pod := sshFixture(t)
	name := pod.Annotations["voidshell.io/ssh-secret"]
	if errors := validation.IsDNS1123Subdomain(name); len(errors) != 0 {
		t.Fatalf("invalid secret name: %v", errors)
	}
	secret, err := m.client.CoreV1().Secrets("test").Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(secret, pod) || secret.Immutable == nil || !*secret.Immutable {
		t.Fatal("credentials must be immutable and pod-owned")
	}
	host, err := ssh.ParsePrivateKey(secret.Data["host-key"])
	if err != nil {
		t.Fatal(err)
	}
	client, err := ssh.ParsePrivateKey(secret.Data["client-key"])
	if err != nil {
		t.Fatal(err)
	}
	authorized, _, _, _, err := ssh.ParseAuthorizedKey(secret.Data["authorized_keys"])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(client.PublicKey().Marshal(), authorized.Marshal()) || bytes.Equal(host.PublicKey().Marshal(), client.PublicKey().Marshal()) {
		t.Fatal("host and client key isolation failed")
	}
	if err := m.ensureSSHCredentials(ctx, id, workspace.StorageEphemeral); err != nil {
		t.Fatal(err)
	}
	again, _ := m.client.CoreV1().Secrets("test").Get(ctx, name, metav1.GetOptions{})
	if !bytes.Equal(secret.Data["host-key"], again.Data["host-key"]) {
		t.Fatal("credentials rotated within a pod lifetime")
	}
	volume := pod.Spec.Volumes[1].Secret
	for _, item := range volume.Items {
		if item.Key == "client-key" {
			t.Fatal("client private key exposed to workspace")
		}
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("workspace API token must not be mounted")
	}
	other := buildPod(id, workspace.StorageEphemeral, Config{Namespace: "test", Backend: "ssh"})
	if other.Annotations["voidshell.io/ssh-secret"] == name {
		t.Fatal("pod recreation must rotate credentials reference")
	}
	// Reusing a name with a different owner must fail closed.
	pod.UID = "replacement-pod"
	if _, err := m.client.CoreV1().Pods("test").Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := m.ensureSSHCredentials(ctx, id, workspace.StorageEphemeral); err == nil {
		t.Fatal("accepted another pod's credentials")
	}
	if err := m.prepareSSH(ctx, id, workspace.StorageEphemeral); err == nil {
		t.Fatal("dial accepted another pod's credentials")
	}
}

func TestSSHRejectsExecPodDuringBackendSwitch(t *testing.T) {
	id := workspace.New("alice", "dev")
	pod := buildPod(id, workspace.StorageEphemeral, Config{Namespace: "test"})
	m := New(fake.NewClientset(pod), Config{Namespace: "test", Backend: "ssh"})
	if err := m.ensureSSHCredentials(context.Background(), id, workspace.StorageEphemeral); err == nil {
		t.Fatal("expected backend mismatch error")
	}
}

func TestSSHEndpointLivesOnlyForWorkspaceLease(t *testing.T) {
	m, id, _ := sshFixture(t)
	client := m.client.(*fake.Clientset)
	before := len(client.Actions())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if c, err := m.OpenWorkspaceSSH(ctx, id, workspace.StorageEphemeral); err == nil {
		c.Close()
		t.Fatal("cancelled connection succeeded")
	}
	if got := len(client.Actions()); got != before {
		t.Fatalf("channel creation performed %d API calls", got-before)
	}
	if err := m.DeletePod(context.Background(), id, workspace.StorageEphemeral); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.sshEndpoints.Load(id.PodNameFor(workspace.StorageEphemeral)); ok {
		t.Fatal("endpoint credentials retained after workspace deletion")
	}
	if c, err := m.OpenWorkspaceSSH(context.Background(), id, workspace.StorageEphemeral); err == nil {
		c.Close()
		t.Fatal("deleted workspace was dialed")
	}
}

func TestSSHHandshakeCancellation(t *testing.T) {
	m, id, _ := sshFixture(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	value, _ := m.sshEndpoints.Load(id.PodNameFor(workspace.StorageEphemeral))
	endpoint := value.(*sshEndpoint)
	endpoint.address = ln.Addr().String()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		client, err := m.OpenWorkspaceSSH(ctx, id, workspace.StorageEphemeral)
		if err == nil {
			client.Close()
		}
		result <- err
	}()
	select {
	case conn := <-accepted:
		defer conn.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("SSH did not connect")
	}
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("cancelled handshake succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SSH handshake ignored cancellation")
	}
}
