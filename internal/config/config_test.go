package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") error: %v", err)
	}
	if cfg.SSH.Port != 2222 {
		t.Errorf("default SSH port: got %d, want 2222", cfg.SSH.Port)
	}
	if cfg.Kubernetes.GuestNamespace != "voidshell-workspaces" {
		t.Errorf("default guest namespace: got %q, want %q", cfg.Kubernetes.GuestNamespace, "voidshell-workspaces")
	}
	if cfg.Kubernetes.StorageSize != "5Gi" {
		t.Errorf("default storage size: got %q, want %q", cfg.Kubernetes.StorageSize, "5Gi")
	}
	if cfg.Workspace.ShellImage != "ubuntu:22.04" {
		t.Errorf("default shell image: got %q, want %q", cfg.Workspace.ShellImage, "ubuntu:22.04")
	}
	if len(cfg.Workspace.ShellCommand) != 1 || cfg.Workspace.ShellCommand[0] != "/bin/bash" {
		t.Errorf("default shell command: got %v, want [/bin/bash]", cfg.Workspace.ShellCommand)
	}
}

func TestLoadFromFile(t *testing.T) {
	content := `
ssh:
  port: 2200
  hostKeyPath: /etc/voidshell/host_key
auth:
  allowedGitHubUsers:
    - alice
    - bob
kubernetes:
  guestNamespace: shells
  storageClass: fast-ssd
  storageSize: 10Gi
workspace:
  shellImage: debian:12
  shellCommand:
    - /bin/sh
    - -l
`
	path := filepath.Join(t.TempDir(), "voidshell.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%q) error: %v", path, err)
	}

	if cfg.SSH.Port != 2200 {
		t.Errorf("SSH port: got %d, want 2200", cfg.SSH.Port)
	}
	if cfg.SSH.HostKeyPath != "/etc/voidshell/host_key" {
		t.Errorf("host key path: got %q", cfg.SSH.HostKeyPath)
	}
	if len(cfg.Auth.AllowedGitHubUsers) != 2 || cfg.Auth.AllowedGitHubUsers[0] != "alice" {
		t.Errorf("allowed users: got %v", cfg.Auth.AllowedGitHubUsers)
	}
	if cfg.Kubernetes.GuestNamespace != "shells" {
		t.Errorf("guest namespace: got %q", cfg.Kubernetes.GuestNamespace)
	}
	if cfg.Kubernetes.StorageClass != "fast-ssd" {
		t.Errorf("storage class: got %q", cfg.Kubernetes.StorageClass)
	}
	if cfg.Kubernetes.StorageSize != "10Gi" {
		t.Errorf("storage size: got %q", cfg.Kubernetes.StorageSize)
	}
	if cfg.Workspace.ShellImage != "debian:12" {
		t.Errorf("shell image: got %q", cfg.Workspace.ShellImage)
	}
	if len(cfg.Workspace.ShellCommand) != 2 {
		t.Errorf("shell command: got %v", cfg.Workspace.ShellCommand)
	}
}

func TestLoadEnvOverrides(t *testing.T) {
	t.Setenv("VOIDSHELL_SSH_PORT", "2300")
	t.Setenv("VOIDSHELL_K8S_GUEST_NAMESPACE", "env-ns")
	t.Setenv("VOIDSHELL_AUTH_ALLOWED_USERS", "carol, dave")
	t.Setenv("VOIDSHELL_WORKSPACE_SHELL_IMAGE", "alpine:3.19")
	t.Setenv("VOIDSHELL_WORKSPACE_SHELL_COMMAND", "/bin/sh,-l")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg.SSH.Port != 2300 {
		t.Errorf("SSH port from env: got %d, want 2300", cfg.SSH.Port)
	}
	if cfg.Kubernetes.GuestNamespace != "env-ns" {
		t.Errorf("guest namespace from env: got %q", cfg.Kubernetes.GuestNamespace)
	}
	if len(cfg.Auth.AllowedGitHubUsers) != 2 || cfg.Auth.AllowedGitHubUsers[1] != "dave" {
		t.Errorf("allowed users from env: got %v", cfg.Auth.AllowedGitHubUsers)
	}
	if cfg.Workspace.ShellImage != "alpine:3.19" {
		t.Errorf("shell image from env: got %q", cfg.Workspace.ShellImage)
	}
	if len(cfg.Workspace.ShellCommand) != 2 || cfg.Workspace.ShellCommand[0] != "/bin/sh" {
		t.Errorf("shell command from env: got %v", cfg.Workspace.ShellCommand)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load("/nonexistent/path/voidshell.yaml")
	if err == nil {
		t.Error("expected error for missing file, got nil")
	}
}
