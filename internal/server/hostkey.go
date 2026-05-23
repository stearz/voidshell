package server

import (
	"context"
	"fmt"
	"os"

	"golang.org/x/crypto/ssh"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/stearz/voidshell/internal/config"
)

// LoadHostKey loads an SSH host key signer from the source specified in cfg.
// If hostKeyPath is set it reads from the filesystem. If hostKeySecret is set
// it reads from the named Kubernetes Secret in namespace. Returns an error if
// neither is configured.
func LoadHostKey(ctx context.Context, cfg config.SSHConfig, k8sClient kubernetes.Interface, namespace string) (ssh.Signer, error) {
	if cfg.HostKeyPath != "" {
		return loadHostKeyFromFile(cfg.HostKeyPath)
	}
	if cfg.HostKeySecret != "" {
		return loadHostKeyFromSecret(ctx, k8sClient, namespace, cfg.HostKeySecret)
	}
	return nil, fmt.Errorf("no host key configured: set ssh.hostKeyPath or ssh.hostKeySecret")
}

func loadHostKeyFromFile(path string) (ssh.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading host key %q: %w", path, err)
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("parsing host key %q: %w", path, err)
	}
	return signer, nil
}

func loadHostKeyFromSecret(ctx context.Context, client kubernetes.Interface, namespace, secretName string) (ssh.Signer, error) {
	secret, err := client.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting host key secret %q: %w", secretName, err)
	}
	// Try well-known key names in order of preference.
	for _, k := range []string{"ssh_host_ed25519_key", "ssh_host_rsa_key", "hostkey", "key"} {
		if data, ok := secret.Data[k]; ok {
			signer, err := ssh.ParsePrivateKey(data)
			if err != nil {
				return nil, fmt.Errorf("parsing host key from secret %q key %q: %w", secretName, k, err)
			}
			return signer, nil
		}
	}
	return nil, fmt.Errorf("host key secret %q has no recognized key field (tried: ssh_host_ed25519_key, ssh_host_rsa_key, hostkey, key)", secretName)
}
