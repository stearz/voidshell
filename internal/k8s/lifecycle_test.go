package k8s

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stearz/voidshell/internal/workspace"
)

func testConfig() Config {
	return Config{
		Namespace:       "test-ns",
		StorageClass:    "standard",
		StorageSize:     "1Gi",
		Image:           "ubuntu:22.04",
		ShellCommand:    []string{"/bin/bash"},
		PodReadyTimeout: 5 * time.Second,
		PodPollInterval: 10 * time.Millisecond,
	}
}

// setPodPhase updates a pod's status phase in the fake client.
func setPodPhase(t *testing.T, client *fake.Clientset, ns, name string, phase corev1.PodPhase) {
	t.Helper()
	pod, err := client.CoreV1().Pods(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("setPodPhase: get pod %q: %v", name, err)
	}
	pod.Status.Phase = phase
	if _, err := client.CoreV1().Pods(ns).UpdateStatus(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("setPodPhase: update pod %q: %v", name, err)
	}
}

func ensureWorkspaceRunning(t *testing.T, mgr *Manager, client *fake.Clientset, id workspace.Identity, mode workspace.StorageMode) {
	t.Helper()
	errCh := make(chan error, 1)
	go func() { errCh <- mgr.EnsureWorkspace(context.Background(), id, mode) }()

	deadline := time.Now().Add(time.Second)
	for {
		if _, err := client.CoreV1().Pods("test-ns").Get(context.Background(), id.PodNameFor(mode), metav1.GetOptions{}); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("workspace pod was not created")
		}
		time.Sleep(time.Millisecond)
	}
	setPodPhase(t, client, "test-ns", id.PodNameFor(mode), corev1.PodRunning)
	if err := <-errCh; err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}
}

func TestEnsureWorkspace_EphemeralUsesEmptyDirWithoutPVC(t *testing.T) {
	client := fake.NewSimpleClientset()
	mgr := New(client, testConfig())
	id := workspace.New("alice", "dev")
	ctx := context.Background()

	ensureWorkspaceRunning(t, mgr, client, id, workspace.StorageEphemeral)

	pvcs, err := client.CoreV1().PersistentVolumeClaims("test-ns").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pvcs.Items) != 0 {
		t.Errorf("PVCs = %d, want 0", len(pvcs.Items))
	}

	pod, err := client.CoreV1().Pods("test-ns").Get(ctx, id.PodNameFor(workspace.StorageEphemeral), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("workspace pod not found: %v", err)
	}
	if pod.Spec.Volumes[0].EmptyDir == nil {
		t.Error("workspace volume is not an EmptyDir")
	}
}

func TestEnsureWorkspace_PersistentUsesPVC(t *testing.T) {
	client := fake.NewSimpleClientset()
	mgr := New(client, testConfig())
	id := workspace.New("alice", "dev")
	ctx := context.Background()

	ensureWorkspaceRunning(t, mgr, client, id, workspace.StoragePersistent)

	if _, err := client.CoreV1().PersistentVolumeClaims("test-ns").Get(ctx, id.PVCName(), metav1.GetOptions{}); err != nil {
		t.Fatalf("persistent workspace PVC not found: %v", err)
	}
	pod, err := client.CoreV1().Pods("test-ns").Get(ctx, id.PodNameFor(workspace.StoragePersistent), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("workspace pod not found: %v", err)
	}
	if pod.Spec.Volumes[0].PersistentVolumeClaim == nil {
		t.Error("workspace volume is not a PVC")
	}
}

func TestEnsurePod_StorageModesUseSeparatePods(t *testing.T) {
	client := fake.NewSimpleClientset()
	mgr := New(client, testConfig())
	id := workspace.New("alice", "project")
	ctx := context.Background()

	if err := mgr.ensurePod(ctx, id, workspace.StorageEphemeral); err != nil {
		t.Fatalf("ensure ephemeral pod: %v", err)
	}
	if err := mgr.ensurePod(ctx, id, workspace.StoragePersistent); err != nil {
		t.Fatalf("ensure persistent pod: %v", err)
	}

	ephemeral, err := client.CoreV1().Pods("test-ns").Get(ctx, id.PodNameFor(workspace.StorageEphemeral), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("ephemeral pod not found: %v", err)
	}
	persistent, err := client.CoreV1().Pods("test-ns").Get(ctx, id.PodNameFor(workspace.StoragePersistent), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("persistent pod not found: %v", err)
	}
	if ephemeral.Spec.Volumes[0].EmptyDir == nil {
		t.Error("ephemeral pod does not use EmptyDir")
	}
	if persistent.Spec.Volumes[0].PersistentVolumeClaim == nil {
		t.Error("persistent pod does not use a PVC")
	}
}

// TestEnsurePVC verifies that a PVC is created with the correct spec.
func TestEnsurePVC(t *testing.T) {
	client := fake.NewSimpleClientset()
	mgr := New(client, testConfig())
	id := workspace.New("alice", "dev")

	if err := mgr.ensurePVC(context.Background(), id); err != nil {
		t.Fatalf("ensurePVC: %v", err)
	}

	pvc, err := client.CoreV1().PersistentVolumeClaims("test-ns").Get(context.Background(), id.PVCName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("PVC not found after create: %v", err)
	}
	if pvc.Name != id.PVCName() {
		t.Errorf("PVC name = %q, want %q", pvc.Name, id.PVCName())
	}
	if pvc.Labels[workspaceLabel] != id.WorkspaceID() {
		t.Errorf("PVC label = %q, want %q", pvc.Labels[workspaceLabel], id.WorkspaceID())
	}
	if *pvc.Spec.StorageClassName != "standard" {
		t.Errorf("StorageClass = %q, want %q", *pvc.Spec.StorageClassName, "standard")
	}
	if pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("AccessMode = %v, want ReadWriteOnce", pvc.Spec.AccessModes[0])
	}
}

func TestEnsurePVC_UsesDefaultStorageClassWhenUnset(t *testing.T) {
	client := fake.NewSimpleClientset()
	cfg := testConfig()
	cfg.StorageClass = ""
	mgr := New(client, cfg)
	id := workspace.New("alice", "dev")

	if err := mgr.ensurePVC(context.Background(), id); err != nil {
		t.Fatalf("ensurePVC: %v", err)
	}
	pvc, err := client.CoreV1().PersistentVolumeClaims("test-ns").Get(context.Background(), id.PVCName(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pvc.Spec.StorageClassName != nil {
		t.Errorf("StorageClassName = %q, want nil", *pvc.Spec.StorageClassName)
	}
}

// TestEnsurePVC_Idempotent verifies that calling ensurePVC twice does not error
// and does not create a duplicate PVC.
func TestEnsurePVC_Idempotent(t *testing.T) {
	client := fake.NewSimpleClientset()
	mgr := New(client, testConfig())
	id := workspace.New("alice", "dev")

	for i := range 2 {
		if err := mgr.ensurePVC(context.Background(), id); err != nil {
			t.Fatalf("ensurePVC call %d: %v", i+1, err)
		}
	}

	pvcs, err := client.CoreV1().PersistentVolumeClaims("test-ns").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pvcs.Items) != 1 {
		t.Errorf("expected 1 PVC, got %d", len(pvcs.Items))
	}
}

// TestEnsurePod verifies that a pod is created with the correct spec.
func TestEnsurePod(t *testing.T) {
	client := fake.NewSimpleClientset()
	mgr := New(client, testConfig())
	id := workspace.New("alice", "dev")

	if err := mgr.ensurePod(context.Background(), id, workspace.StoragePersistent); err != nil {
		t.Fatalf("ensurePod: %v", err)
	}

	pod, err := client.CoreV1().Pods("test-ns").Get(context.Background(), id.PodNameFor(workspace.StoragePersistent), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("pod not found after create: %v", err)
	}
	if pod.Name != id.PodNameFor(workspace.StoragePersistent) {
		t.Errorf("pod name = %q, want %q", pod.Name, id.PodNameFor(workspace.StoragePersistent))
	}
	if pod.Labels[workspaceLabel] != id.WorkspaceID() {
		t.Errorf("pod label = %q, want %q", pod.Labels[workspaceLabel], id.WorkspaceID())
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %q, want Never", pod.Spec.RestartPolicy)
	}

	c := pod.Spec.Containers[0]
	if c.Image != "ubuntu:22.04" {
		t.Errorf("image = %q, want ubuntu:22.04", c.Image)
	}
	if c.VolumeMounts[0].MountPath != workspaceMountPath {
		t.Errorf("mount path = %q, want %q", c.VolumeMounts[0].MountPath, workspaceMountPath)
	}
	if pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != id.PVCName() {
		t.Errorf("PVC claim = %q, want %q", pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName, id.PVCName())
	}
}

// TestEnsurePod_Idempotent verifies that calling ensurePod twice does not error.
func TestEnsurePod_Idempotent(t *testing.T) {
	client := fake.NewSimpleClientset()
	mgr := New(client, testConfig())
	id := workspace.New("alice", "dev")

	for i := range 2 {
		if err := mgr.ensurePod(context.Background(), id, workspace.StoragePersistent); err != nil {
			t.Fatalf("ensurePod call %d: %v", i+1, err)
		}
	}

	pods, err := client.CoreV1().Pods("test-ns").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 1 {
		t.Errorf("expected 1 pod, got %d", len(pods.Items))
	}
}

// TestWaitForPodReady_Running verifies that waitForPodReady returns nil
// immediately when the pod is already Running.
func TestWaitForPodReady_Running(t *testing.T) {
	client := fake.NewSimpleClientset()
	mgr := New(client, testConfig())
	id := workspace.New("alice", "dev")

	if err := mgr.ensurePod(context.Background(), id, workspace.StoragePersistent); err != nil {
		t.Fatal(err)
	}
	setPodPhase(t, client, "test-ns", id.PodNameFor(workspace.StoragePersistent), corev1.PodRunning)

	if err := mgr.waitForPodReady(context.Background(), id, workspace.StoragePersistent); err != nil {
		t.Errorf("waitForPodReady: %v", err)
	}
}

// TestWaitForPodReady_Failed verifies that waitForPodReady returns an error
// when the pod enters Failed phase.
func TestWaitForPodReady_Failed(t *testing.T) {
	client := fake.NewSimpleClientset()
	mgr := New(client, testConfig())
	id := workspace.New("alice", "dev")

	if err := mgr.ensurePod(context.Background(), id, workspace.StoragePersistent); err != nil {
		t.Fatal(err)
	}
	setPodPhase(t, client, "test-ns", id.PodNameFor(workspace.StoragePersistent), corev1.PodFailed)

	if err := mgr.waitForPodReady(context.Background(), id, workspace.StoragePersistent); err == nil {
		t.Error("waitForPodReady returned nil for a Failed pod, want error")
	}
}

// TestWaitForPodReady_Timeout verifies that waitForPodReady returns an error
// when the context deadline is exceeded before the pod becomes Ready.
func TestWaitForPodReady_Timeout(t *testing.T) {
	client := fake.NewSimpleClientset()
	mgr := New(client, testConfig())
	id := workspace.New("alice", "dev")

	if err := mgr.ensurePod(context.Background(), id, workspace.StoragePersistent); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if err := mgr.waitForPodReady(ctx, id, workspace.StoragePersistent); err == nil {
		t.Error("waitForPodReady returned nil on timeout, want error")
	}
}

// TestDeletePod_KeepsPVC verifies that DeletePod removes the pod and leaves the PVC.
func TestDeletePod_KeepsPVC(t *testing.T) {
	client := fake.NewSimpleClientset()
	mgr := New(client, testConfig())
	id := workspace.New("alice", "dev")
	ctx := context.Background()

	if err := mgr.ensurePVC(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := mgr.ensurePod(ctx, id, workspace.StoragePersistent); err != nil {
		t.Fatal(err)
	}

	if err := mgr.DeletePod(ctx, id, workspace.StoragePersistent); err != nil {
		t.Fatalf("DeletePod: %v", err)
	}

	if _, err := client.CoreV1().Pods("test-ns").Get(ctx, id.PodNameFor(workspace.StoragePersistent), metav1.GetOptions{}); err == nil {
		t.Error("pod still exists after DeletePod")
	}
	if _, err := client.CoreV1().PersistentVolumeClaims("test-ns").Get(ctx, id.PVCName(), metav1.GetOptions{}); err != nil {
		t.Errorf("PVC was removed unexpectedly: %v", err)
	}
}

// TestDeletePod_Idempotent verifies that deleting a non-existent pod is not an error.
func TestDeletePod_Idempotent(t *testing.T) {
	client := fake.NewSimpleClientset()
	mgr := New(client, testConfig())
	id := workspace.New("alice", "dev")

	if err := mgr.DeletePod(context.Background(), id, workspace.StoragePersistent); err != nil {
		t.Errorf("DeletePod on missing pod = %v, want nil", err)
	}
}

// TestIsolation verifies that different workspace identities produce distinct
// PVCs and pods, satisfying the acceptance criteria for isolation.
func TestIsolation(t *testing.T) {
	client := fake.NewSimpleClientset()
	mgr := New(client, testConfig())
	ctx := context.Background()

	identities := []workspace.Identity{
		workspace.New("stearz", "someuser"), // different github, same ssh
		workspace.New("alice", "someuser"),  // different github, same ssh
		workspace.New("alice", "other"),     // same github, different ssh
	}

	for _, id := range identities {
		if err := mgr.ensurePVC(ctx, id); err != nil {
			t.Fatalf("ensurePVC(%v): %v", id, err)
		}
		if err := mgr.ensurePod(ctx, id, workspace.StoragePersistent); err != nil {
			t.Fatalf("ensurePod(%v): %v", id, err)
		}
	}

	pvcs, _ := client.CoreV1().PersistentVolumeClaims("test-ns").List(ctx, metav1.ListOptions{})
	if len(pvcs.Items) != len(identities) {
		t.Errorf("expected %d PVCs, got %d", len(identities), len(pvcs.Items))
	}

	pods, _ := client.CoreV1().Pods("test-ns").List(ctx, metav1.ListOptions{})
	if len(pods.Items) != len(identities) {
		t.Errorf("expected %d pods, got %d", len(identities), len(pods.Items))
	}
}
