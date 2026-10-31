// Package k8s manages the Kubernetes lifecycle of voidshell workspace pods and optional PVCs.
//
// Required RBAC for the voidshell ServiceAccount (scoped to the guest namespace):
//
//	pods:                    get, list, create, delete
//	pods/log:                get
//	pods/exec:               create (kubernetes backend only)
//	pods/portforward:        create (kubernetes backend only)
//	secrets:                 get, create (ssh backend only)
//	persistentvolumeclaims:  get, create
//
// The guest namespace must pre-exist; voidshell does not create it.
package k8s

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/stearz/voidshell/internal/workspace"
)

const (
	workspaceMountPath  = "/home/workspace"
	workspaceLabel      = "voidshell.io/workspace-id"
	defaultPollInterval = 2 * time.Second
	defaultReadyTimeout = 2 * time.Minute
)

// Config holds workspace lifecycle configuration derived from the top-level config.
type Config struct {
	Backend string
	// Namespace is the pre-existing guest namespace for pods and PVCs.
	Namespace string
	// StorageClass is the PVC storage class (e.g. "longhorn").
	StorageClass string
	// StorageSize is the requested PVC capacity (e.g. "5Gi").
	StorageSize string
	// Image is the OCI image run in workspace pods.
	Image string
	// ShellCommand is executed once per SSH shell session, independently of PID 1.
	ShellCommand []string
	// PodReadyTimeout bounds workspace provisioning and container readiness.
	// Defaults to 2 minutes when zero.
	PodReadyTimeout time.Duration
	// PodPollInterval controls how often pod status is checked while waiting.
	// Defaults to 2 seconds when zero. Override in tests to speed things up.
	PodPollInterval time.Duration
}

// Manager manages the Kubernetes lifecycle of workspace PVCs and shell pods.
type Manager struct {
	client       kubernetes.Interface
	cfg          Config
	restCfg      *rest.Config // nil when created via New (test mode)
	sshEndpoints sync.Map     // pod name -> *sshEndpoint, valid for one workspace lease lifetime
}

// New creates a Manager with the given client and config. Use this in tests
// with a fake client; use NewFromKubeconfig in production.
func New(client kubernetes.Interface, cfg Config) *Manager {
	return &Manager{client: client, cfg: cfg}
}

// Client returns the underlying Kubernetes client interface.
func (m *Manager) Client() kubernetes.Interface {
	return m.client
}

// NewFromKubeconfig creates a Manager using in-cluster config, falling back to
// the kubeconfig at kubeconfigPath (empty = default ~/.kube/config).
func NewFromKubeconfig(kubeconfigPath string, cfg Config) (*Manager, error) {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		lr := clientcmd.NewDefaultClientConfigLoadingRules()
		if kubeconfigPath != "" {
			lr.ExplicitPath = kubeconfigPath
		}
		restCfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			lr, &clientcmd.ConfigOverrides{},
		).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("building kubeconfig: %w", err)
		}
	}
	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("building Kubernetes client: %w", err)
	}
	return &Manager{client: client, cfg: cfg, restCfg: restCfg}, nil
}

// EnsureWorkspace creates a workspace pod and, for persistent workspaces, a PVC,
// then waits for the workspace container to be ready.
func (m *Manager) EnsureWorkspace(ctx context.Context, id workspace.Identity, mode workspace.StorageMode) error {
	timeout := m.cfg.PodReadyTimeout
	if timeout == 0 {
		timeout = defaultReadyTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if mode == workspace.StoragePersistent {
		if err := m.ensurePVC(ctx, id); err != nil {
			return fmt.Errorf("ensuring PVC: %w", err)
		}
	}
	if err := m.ensurePod(ctx, id, mode); err != nil {
		return fmt.Errorf("ensuring pod: %w", err)
	}
	if m.DirectSSHEnabled() {
		if err := m.ensureSSHCredentials(ctx, id, mode); err != nil {
			return fmt.Errorf("ensuring SSH credentials: %w", err)
		}
	}
	if err := m.waitForPodReady(ctx, id, mode); err != nil {
		return fmt.Errorf("waiting for pod ready: %w", err)
	}
	if m.DirectSSHEnabled() {
		if err := m.prepareSSH(ctx, id, mode); err != nil {
			return fmt.Errorf("preparing SSH endpoint: %w", err)
		}
	}
	return nil
}

// DeletePod deletes the workspace pod for the requested storage mode, leaving a
// persistent-mode PVC intact for the next reconnect. It is idempotent: a missing
// pod is not an error.
func (m *Manager) DeletePod(ctx context.Context, id workspace.Identity, mode workspace.StorageMode) error {
	m.sshEndpoints.Delete(id.PodNameFor(mode))
	err := m.client.CoreV1().Pods(m.cfg.Namespace).Delete(ctx, id.PodNameFor(mode), metav1.DeleteOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	// Deletion is asynchronous. Do not let a reconnect reuse a terminating pod.
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, err := m.client.CoreV1().Pods(m.cfg.Namespace).Get(ctx, id.PodNameFor(mode), metav1.GetOptions{})
		if errors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (m *Manager) ensurePVC(ctx context.Context, id workspace.Identity) error {
	_, err := m.client.CoreV1().PersistentVolumeClaims(m.cfg.Namespace).
		Get(ctx, id.PVCName(), metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("getting PVC %q: %w", id.PVCName(), err)
	}
	sc := m.cfg.StorageClass
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      id.PVCName(),
			Namespace: m.cfg.Namespace,
			Labels:    map[string]string{workspaceLabel: id.WorkspaceID()},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(m.cfg.StorageSize),
				},
			},
		},
	}
	if sc != "" {
		pvc.Spec.StorageClassName = &sc
	}
	_, err = m.client.CoreV1().PersistentVolumeClaims(m.cfg.Namespace).
		Create(ctx, pvc, metav1.CreateOptions{})
	return err
}

func (m *Manager) ensurePod(ctx context.Context, id workspace.Identity, mode workspace.StorageMode) error {
	for {
		pod, err := m.client.CoreV1().Pods(m.cfg.Namespace).Get(ctx, id.PodNameFor(mode), metav1.GetOptions{})
		if errors.IsNotFound(err) {
			_, err = m.client.CoreV1().Pods(m.cfg.Namespace).Create(ctx, buildPod(id, mode, m.cfg), metav1.CreateOptions{})
			if errors.IsAlreadyExists(err) {
				continue
			}
			return err
		}
		if err != nil {
			return fmt.Errorf("getting pod %q: %w", id.PodNameFor(mode), err)
		}
		if pod.DeletionTimestamp == nil {
			if (pod.Labels["voidshell.io/backend"] == "ssh") != m.DirectSSHEnabled() {
				return fmt.Errorf("workspace pod backend differs from configured backend; remove old pods before switching")
			}
			return nil
		}
		// A previous cleanup may have timed out. Never execute in a dying pod.
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (m *Manager) waitForPodReady(ctx context.Context, id workspace.Identity, mode workspace.StorageMode) error {
	check := func() (bool, error) {
		pod, err := m.client.CoreV1().Pods(m.cfg.Namespace).
			Get(ctx, id.PodNameFor(mode), metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("getting pod %q: %w", id.PodNameFor(mode), err)
		}
		switch pod.Status.Phase {
		case corev1.PodRunning:
			for _, status := range pod.Status.ContainerStatuses {
				if status.Name == "workspace" && status.Ready {
					return true, nil
				}
			}
		case corev1.PodFailed, corev1.PodSucceeded:
			return false, fmt.Errorf("pod %q reached terminal phase %s", id.PodNameFor(mode), pod.Status.Phase)
		}
		return false, nil
	}

	// Fast path: pod is already in a terminal or running state.
	if done, err := check(); err != nil || done {
		return err
	}

	interval := m.cfg.PodPollInterval
	if interval == 0 {
		interval = defaultPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("pod %q not ready within timeout: %w", id.PodNameFor(mode), ctx.Err())
		case <-ticker.C:
			if done, err := check(); err != nil || done {
				return err
			}
		}
	}
}

func buildPod(id workspace.Identity, mode workspace.StorageMode, cfg Config) *corev1.Pod {
	uid := int64(1000)
	grace := int64(1)
	nonRoot := true
	noEscalation := false
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      id.PodNameFor(mode),
			Namespace: cfg.Namespace,
			Labels:    map[string]string{workspaceLabel: id.WorkspaceID()},
		},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken:  func() *bool { v := false; return &v }(),
			RestartPolicy:                 corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: &grace,
			SecurityContext:               &corev1.PodSecurityContext{FSGroup: &uid},
			Containers: []corev1.Container{{
				Name:  "workspace",
				Image: cfg.Image,
				// Keep PID 1 independent of SSH sessions. Trap TERM for fast cleanup.
				Command:    []string{"/bin/sh", "-c", "trap 'exit 0' TERM INT; while :; do sleep 3600 & wait $!; done"},
				WorkingDir: workspaceMountPath,
				Env: []corev1.EnvVar{
					{Name: "HOME", Value: workspaceMountPath},
					{Name: "VOIDSHELL_USER", Value: id.SSHUser},
					{Name: "USER", Value: "voidshell"},
					{Name: "LOGNAME", Value: "voidshell"},
				},
				SecurityContext: &corev1.SecurityContext{
					RunAsUser:                &uid,
					RunAsNonRoot:             &nonRoot,
					AllowPrivilegeEscalation: &noEscalation,
					Capabilities: &corev1.Capabilities{
						Drop: []corev1.Capability{"ALL"},
					},
				},
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "home",
					MountPath: workspaceMountPath,
				}},
			}},
			Volumes: []corev1.Volume{{
				Name:         "home",
				VolumeSource: workspaceVolumeSource(id, mode),
			}},
		},
	}
	if cfg.Backend == "ssh" {
		configureSSHPod(pod)
	}
	return pod
}

func workspaceVolumeSource(id workspace.Identity, mode workspace.StorageMode) corev1.VolumeSource {
	if mode == workspace.StoragePersistent {
		return corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: id.PVCName()},
		}
	}
	return corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
}
