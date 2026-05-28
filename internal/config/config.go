package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the top-level voidshell configuration.
type Config struct {
	SSH        SSHConfig        `yaml:"ssh"`
	Auth       AuthConfig       `yaml:"auth"`
	Kubernetes KubernetesConfig `yaml:"kubernetes"`
	Workspace  WorkspaceConfig  `yaml:"workspace"`
}

// SSHConfig configures the SSH listener.
type SSHConfig struct {
	// Port is the TCP port the SSH server listens on.
	Port int `yaml:"port"`
	// HostKeyPath is a path to a PEM-encoded host key file.
	HostKeyPath string `yaml:"hostKeyPath"`
	// HostKeySecret is the name of a Kubernetes Secret that holds the host key.
	HostKeySecret string `yaml:"hostKeySecret"`
}

// AuthConfig defines who is permitted to open a workspace.
type AuthConfig struct {
	// AllowedGitHubUsers is the list of GitHub usernames that may connect.
	// An empty list denies everyone.
	AllowedGitHubUsers []string `yaml:"allowedGitHubUsers"`
	// KeyCacheTTL is how long fetched GitHub key lists are cached before being
	// re-fetched. Removed keys stop working within one TTL. Accepts any
	// duration string valid for time.ParseDuration (e.g. "5m", "1h").
	KeyCacheTTL string `yaml:"keyCacheTTL"`
}

// KubernetesConfig configures workspace provisioning.
type KubernetesConfig struct {
	// GuestNamespace is the namespace where workspace pods and PVCs are created.
	GuestNamespace string `yaml:"guestNamespace"`
	// StorageClass is the PVC storage class used for workspace volumes.
	StorageClass string `yaml:"storageClass"`
	// StorageSize is the requested PVC size (e.g. "5Gi").
	StorageSize string `yaml:"storageSize"`
}

// WorkspaceConfig describes the workspace container.
type WorkspaceConfig struct {
	// Image is the OCI image used for the workspace pod.
	Image string `yaml:"image"`
	// ShellCommand is the entrypoint executed inside the workspace pod.
	ShellCommand []string `yaml:"shellCommand"`
}

// Load reads configuration from the optional YAML file at path and then
// applies any VOIDSHELL_* environment variable overrides. When path is
// empty the file step is skipped and only defaults + env vars are used.
func Load(path string) (*Config, error) {
	cfg := defaults()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading config file %q: %w", path, err)
		}
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("parsing config file %q: %w", path, err)
		}
	}

	applyEnv(&cfg)
	return &cfg, nil
}

func defaults() Config {
	return Config{
		SSH: SSHConfig{
			Port: 2222,
		},
		Auth: AuthConfig{
			KeyCacheTTL: "5m",
		},
		Kubernetes: KubernetesConfig{
			GuestNamespace: "voidshell-workspaces",
			StorageClass:   "standard",
			StorageSize:    "5Gi",
		},
		Workspace: WorkspaceConfig{
			Image:        "ghcr.io/stearz/voidshell-workspace:main",
			ShellCommand: []string{"/bin/bash"},
		},
	}
}

// applyEnv overrides config fields from environment variables.
// This lets individual values (e.g. secrets) be injected via Kubernetes
// Secret env refs without rewriting the whole config file.
func applyEnv(c *Config) {
	if v := os.Getenv("VOIDSHELL_SSH_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			c.SSH.Port = p
		}
	}
	if v := os.Getenv("VOIDSHELL_SSH_HOST_KEY_PATH"); v != "" {
		c.SSH.HostKeyPath = v
	}
	if v := os.Getenv("VOIDSHELL_SSH_HOST_KEY_SECRET"); v != "" {
		c.SSH.HostKeySecret = v
	}
	if v := os.Getenv("VOIDSHELL_AUTH_ALLOWED_USERS"); v != "" {
		c.Auth.AllowedGitHubUsers = splitTrimmed(v, ",")
	}
	if v := os.Getenv("VOIDSHELL_AUTH_KEY_CACHE_TTL"); v != "" {
		c.Auth.KeyCacheTTL = v
	}
	if v := os.Getenv("VOIDSHELL_K8S_GUEST_NAMESPACE"); v != "" {
		c.Kubernetes.GuestNamespace = v
	}
	if v := os.Getenv("VOIDSHELL_K8S_STORAGE_CLASS"); v != "" {
		c.Kubernetes.StorageClass = v
	}
	if v := os.Getenv("VOIDSHELL_K8S_STORAGE_SIZE"); v != "" {
		c.Kubernetes.StorageSize = v
	}
	if v := os.Getenv("VOIDSHELL_WORKSPACE_IMAGE"); v != "" {
		c.Workspace.Image = v
	}
	if v := os.Getenv("VOIDSHELL_WORKSPACE_SHELL_COMMAND"); v != "" {
		c.Workspace.ShellCommand = splitTrimmed(v, ",")
	}
}

func splitTrimmed(s, sep string) []string {
	parts := strings.Split(s, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
