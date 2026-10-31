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
    --from-file=host-key=/tmp/voidshell_host_key \
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
  image: ghcr.io/stearz/voidshell-workspace:main
  shellCommand: [/bin/bash, -l]

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
| Role + RoleBinding | `kubernetes.guestNamespace` | pods (get/list/create/delete), pods/log (get), pods/exec and pods/portforward (create), PVCs (get/create) |

The voidshell ServiceAccount in `.Release.Namespace` is bound cross-namespace to the guest Role. It holds **no cluster-scoped permissions**.

The listener host key Secret is mounted as a file. With `workspace.backend: ssh`,
the guest Role replaces exec/port-forward permissions with `secrets/get,create`
for per-pod authentication and host keys.

## Values reference

| Key | Default | Description |
|---|---|---|
| `image.repository` | `ghcr.io/stearz/voidshell` | Container image |
| `image.tag` | chart `appVersion` | Image tag |
| `ssh.port` | `2222` | SSH listen port |
| `ssh.hostKeySecret` | `voidshell-host-key` | Name of K8s Secret containing the SSH host key |
| `auth.allowedGitHubUsers` | `[]` | GitHub usernames permitted to connect |
| `auth.keyCacheTTL` | `5m` | How long GitHub key lists are cached |
| `kubernetes.guestNamespace` | `voidshell-guest` | Namespace for workspace pods and persistent-mode PVCs |
| `kubernetes.storageClass` | `""` | Optional class for `persist.<workspace>` PVCs; empty uses the cluster default |
| `kubernetes.storageSize` | `5Gi` | PVC size for `persist.<workspace>` workspaces |
| `workspace.backend` | `kubernetes` | `kubernetes` Exec API or `ssh` direct pod transport |
| `workspace.image` | `ghcr.io/stearz/voidshell-workspace:main` | Image used for workspace pods |
| `workspace.shellCommand` | `[/bin/bash, -l]` | Shell process started for each session |
| `service.type` | `ClusterIP` | Service type |
| `service.port` | `2222` | Service port |
| `networkPolicy.enabled` | `true` | Create the proxy ingress/egress NetworkPolicy |
| `nodeSelector` | `{}` | Pod node selector (use `kubernetes.io/arch: arm64` for Raspberry Pi) |
| `tolerations` | `[]` | Pod tolerations |
| `affinity` | `{}` | Pod affinity rules |

## Session lifecycle and upgrades

Run one proxy instance per guest namespace. The chart rejects `replicaCount`
other than 1 and uses `Recreate` upgrades because workspace leases are coordinated
in memory. Upgrades interrupt active SSH connections. Graceful shutdown waits for
workspace cleanup; abrupt crashes can leave orphaned pods.

With the default Kubernetes backend, the proxy requires `pods/exec` and `pods/portforward` create permissions in the
guest namespace. Forwarding goes through the Kubernetes API, so no direct
proxy-to-workspace network exception is required. Continue to allow proxy egress
to your API server via `networkPolicy.egressPrivateExceptions`.

When upgrading from the old attach-based session model, inspect and remove idle
workspace pods before reconnecting. PVCs remain intact. Build the updated
workspace image for SFTP, legacy SCP, and rsync binaries. Custom image entrypoints
are bypassed; images need `/bin/sh`, `sleep`, `env`, and the configured shell.

The pod is deleted after its last active shell, command, SFTP, or forwarding
channel closes. `HOME` and the working directory are `/home/workspace`, writable
via pod `fsGroup: 1000`. Only `persist.<workspace>` home volumes survive deletion.
See [ADR 004](../../docs/adr/004-independent-sessions-and-workspace-lifecycle.md).

## Direct SSH requirements

Set `workspace.backend: ssh` and use a workspace image built from this revision.
The proxy must reach workspace pod IPs on TCP 2222. When NetworkPolicy is enabled,
the chart adds proxy egress to matching workspace pods and restricts their ingress
to this release's proxy pods. A compatible CNI is required; other policies can
add permissions. Workspace egress policy is operator-owned. Keep the API-server
private-network exception: provisioning and per-pod Secret reads still use it.

The startup script runs OpenSSH as UID 1000 with all capabilities dropped. It
mounts only the pod's host private key and authorized client public key. Pod-owned
immutable Secrets rotate with pod recreation and are garbage-collected. No keys
are baked into the image or stored on the home PVC. The proxy pins the pod's exact
host key. `workspace.shellCommand` applies only to Kubernetes mode; SSH uses the
image's passwd shell. Review [ADR 005](../../docs/adr/005-direct-ssh-backend.md)
for custom-image requirements and key rotation. Remove old workspace pods when
switching backends, preserving their PVCs.
