// Package k8s manages the Kubernetes lifecycle of voidshell workspace PVCs and pods.
//
// Required RBAC for the voidshell ServiceAccount (scoped to the guest namespace):
//
//	pods:                    get, list, create, delete
//	pods/log:                get
//	pods/attach:             create
//	persistentvolumeclaims:  get, create
//
// The guest namespace must pre-exist; voidshell does not create it.
package k8s

import (
	"context"
	"fmt"
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
	// Namespace is the pre-existing guest namespace for pods and PVCs.
	Namespace string
	// StorageClass is the PVC storage class (e.g. "longhorn").
	StorageClass string
	// StorageSize is the requested PVC capacity (e.g. "5Gi").
	StorageSize string
	// ShellImage is the OCI image run in workspace pods.
	ShellImage string
	// ShellCommand is the entrypoint executed inside the workspace container.
	ShellCommand []string
	// PodReadyTimeout is how long EnsureWorkspace waits for a pod to reach Running.
	// Defaults to 2 minutes when zero.
	PodReadyTimeout time.Duration
	// PodPollInterval controls how often pod status is checked while waiting.
	// Defaults to 2 seconds when zero. Override in tests to speed things up.
	PodPollInterval time.Duration
}

// Manager manages the Kubernetes lifecycle of workspace PVCs and shell pods.
type Manager struct {
	client  kubernetes.Interface
	cfg     Config
	restCfg *rest.Config // nil when created via New (test mode)
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

// EnsureWorkspace creates or reuses the PVC and pod for the given workspace
// identity, then waits for the pod to reach Running phase.
func (m *Manager) EnsureWorkspace(ctx context.Context, id workspace.Identity) error {
	if err := m.ensurePVC(ctx, id); err != nil {
		return fmt.Errorf("ensuring PVC: %w", err)
	}
	if err := m.ensurePod(ctx, id); err != nil {
		return fmt.Errorf("ensuring pod: %w", err)
	}
	timeout := m.cfg.PodReadyTimeout
	if timeout == 0 {
		timeout = defaultReadyTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := m.waitForPodReady(waitCtx, id); err != nil {
		return fmt.Errorf("waiting for pod ready: %w", err)
	}
	return nil
}

// DeletePod deletes the workspace pod, leaving the PVC intact for the next
// reconnect. It is idempotent: a missing pod is not an error.
func (m *Manager) DeletePod(ctx context.Context, id workspace.Identity) error {
	err := m.client.CoreV1().Pods(m.cfg.Namespace).Delete(ctx, id.PodName(), metav1.DeleteOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	return err
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
			StorageClassName: &sc,
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(m.cfg.StorageSize),
				},
			},
		},
	}
	_, err = m.client.CoreV1().PersistentVolumeClaims(m.cfg.Namespace).
		Create(ctx, pvc, metav1.CreateOptions{})
	return err
}

func (m *Manager) ensurePod(ctx context.Context, id workspace.Identity) error {
	_, err := m.client.CoreV1().Pods(m.cfg.Namespace).
		Get(ctx, id.PodName(), metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("getting pod %q: %w", id.PodName(), err)
	}
	_, err = m.client.CoreV1().Pods(m.cfg.Namespace).
		Create(ctx, buildPod(id, m.cfg), metav1.CreateOptions{})
	return err
}

func (m *Manager) waitForPodReady(ctx context.Context, id workspace.Identity) error {
	check := func() (bool, error) {
		pod, err := m.client.CoreV1().Pods(m.cfg.Namespace).
			Get(ctx, id.PodName(), metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("getting pod %q: %w", id.PodName(), err)
		}
		switch pod.Status.Phase {
		case corev1.PodRunning:
			return true, nil
		case corev1.PodFailed, corev1.PodSucceeded:
			return false, fmt.Errorf("pod %q reached terminal phase %s", id.PodName(), pod.Status.Phase)
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
			return fmt.Errorf("pod %q not ready within timeout: %w", id.PodName(), ctx.Err())
		case <-ticker.C:
			if done, err := check(); err != nil || done {
				return err
			}
		}
	}
}

func buildPod(id workspace.Identity, cfg Config) *corev1.Pod {
	uid := int64(1000)
	nonRoot := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      id.PodName(),
			Namespace: cfg.Namespace,
			Labels:    map[string]string{workspaceLabel: id.WorkspaceID()},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:    "workspace",
				Image:   cfg.ShellImage,
				Command: cfg.ShellCommand,
				Env: []corev1.EnvVar{
					{Name: "VOIDSHELL_USER", Value: id.SSHUser},
					{Name: "USER", Value: id.SSHUser},
					{Name: "LOGNAME", Value: id.SSHUser},
				},
				SecurityContext: &corev1.SecurityContext{
					RunAsUser:    &uid,
					RunAsNonRoot: &nonRoot,
				},
				Stdin: true,
				TTY:   true,
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "home",
					MountPath: workspaceMountPath,
				}},
			}},
			Volumes: []corev1.Volume{{
				Name: "home",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: id.PVCName(),
					},
				},
			}},
		},
	}
}
