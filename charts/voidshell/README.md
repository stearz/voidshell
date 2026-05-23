# voidshell Helm chart

SSH-to-Kubernetes workspace proxy with GitHub public key authentication.

## Prerequisites

- Kubernetes 1.26+
- Helm 3.10+
- The guest namespace must exist before installing:
  ```
  kubectl create namespace voidshell-guest
  ```
- An SSH host key secret in the release namespace:
  ```
  ssh-keygen -t ed25519 -f /tmp/voidshell_host_key -N ""
  kubectl create secret generic voidshell-host-key \
    --from-file=ssh_host_ed25519_key=/tmp/voidshell_host_key \
    -n voidshell
  ```

## Minimal homelab values

```yaml
# values-homelab.yaml
auth:
  allowedGitHubUsers:
    - stearz

kubernetes:
  guestNamespace: voidshell-guest
  storageClass: longhorn
  storageSize: 5Gi

workspace:
  shellImage: ubuntu:22.04
  shellCommand: [/bin/bash]

service:
  type: LoadBalancer
  port: 2222

# Target arm64 Raspberry Pi nodes
nodeSelector:
  kubernetes.io/arch: arm64
```

Install with:

```
helm upgrade --install voidshell oci://ghcr.io/stearz/charts/voidshell \
  -n voidshell --create-namespace \
  -f values-homelab.yaml
```

## RBAC

The chart creates:

| Resource | Namespace | Permissions |
|---|---|---|
| Role + RoleBinding | `kubernetes.guestNamespace` | pods (get/list/create/delete), pods/log (get), pods/attach (create), PVCs (get/create) |

The voidshell ServiceAccount in `.Release.Namespace` is bound cross-namespace to the guest Role. It holds **no cluster-scoped permissions**.

The SSH host key secret is mounted as a file — no `secrets/get` API permission is required.

## Values reference

| Key | Default | Description |
|---|---|---|
| `image.repository` | `ghcr.io/stearz/voidshell` | Container image |
| `image.tag` | chart `appVersion` | Image tag |
| `ssh.port` | `2222` | SSH listen port |
| `ssh.hostKeySecret` | `voidshell-host-key` | Name of K8s Secret containing the SSH host key |
| `auth.allowedGitHubUsers` | `[]` | GitHub usernames permitted to connect |
| `auth.keyCacheTTL` | `5m` | How long GitHub key lists are cached |
| `kubernetes.guestNamespace` | `voidshell-guest` | Namespace for workspace pods and PVCs |
| `kubernetes.storageClass` | `longhorn` | Storage class for workspace PVCs |
| `kubernetes.storageSize` | `5Gi` | PVC size per workspace |
| `workspace.shellImage` | `ubuntu:22.04` | Image used for workspace pods |
| `workspace.shellCommand` | `[/bin/bash]` | Entrypoint inside workspace pods |
| `service.type` | `ClusterIP` | Service type |
| `service.port` | `2222` | Service port |
| `networkPolicy.enabled` | `false` | Create a NetworkPolicy restricting SSH ingress |
| `nodeSelector` | `{}` | Pod node selector (use `kubernetes.io/arch: arm64` for Raspberry Pi) |
| `tolerations` | `[]` | Pod tolerations |
| `affinity` | `{}` | Pod affinity rules |
