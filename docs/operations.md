# voidshell — Operations Guide

This document covers the identity model, object naming, Helm values, expected
lifecycle, security/RBAC assumptions, and a manual smoke-test checklist.

---

## Identity and storage selection

A voidshell workspace is identified by the tuple:

```
(github_username, workspace_name)
```

- **`github_username`** is the authenticated GitHub account whose public keys
  were accepted during the SSH handshake. It is the security boundary.
- **`workspace_name`** is selected through the SSH username. It is not an auth
  factor; one GitHub account can select independent workspaces.

The SSH username also selects storage mode:

```text
<workspace>          ephemeral (default)
persist.<workspace>  persistent
```

An ordinary selector creates an `emptyDir`-backed pod and no PVC. A persistent
selector strips `persist.` before deriving the logical workspace name and
creates/reuses its PVC. For example, `persist.devbox` has the logical workspace
name `devbox`.

### Key properties

| Property | Description |
|---|---|
| Two users, same selector | Different workspaces; GitHub user is the discriminator |
| Same user, different logical workspace names | Different workspace identities |
| `devbox` | Disposable `emptyDir` workspace; no PVC is created |
| `persist.devbox` | A PVC-backed `devbox` workspace; data survives pod deletion |
| Unknown SSH public key | Connection rejected before any workspace is touched |

---

## Kubernetes object naming

The stable workspace ID is derived from the logical identity:

```
workspace_id = vs-<normalized-github>-<normalized-workspace>-<hash6>
persistent_pod_name = shell-<workspace_id>
ephemeral_pod_name  = shell-<workspace_id>-ephemeral
pvc_name            = home-<workspace_id>  # persistent mode only
```

The storage-specific pod names prevent an ephemeral and persistent session for
the same logical workspace from attaching to a pod with the wrong volume type.
The persistent pod name remains compatible with the original naming scheme.

**Normalization rules:**
- Lowercased, non-alphanumeric characters replaced with `-`, leading/trailing
  hyphens stripped, truncated to 26 characters per segment.
- A 6-character SHA-256 hash of the raw (un-normalized) identity ensures
  uniqueness even if normalized segments collide.

The workspace ID is always ≤ 63 characters (RFC 1123 DNS label compliant).

---

## Workspace image

voidshell ships a purpose-built workspace image (`Dockerfile.workspace`) that
is strongly recommended over a plain base image. It provides:

- **Homebrew** pre-installed and writable by the session user.
- **Non-root execution** — the container starts as the pre-baked `voidshell`
  user (UID 1000) with `runAsNonRoot: true`; the shell prompt shows the SSH
  username via injected env vars.

### How user identity works

The workspace image pre-bakes a `voidshell` user at UID 1000 (member of the
`brew` group). At pod creation time voidshell sets `securityContext.runAsUser:
1000` and `runAsNonRoot: true` so the container starts directly as that user —
it never runs as root.

Three env vars are injected into the pod at creation time:

| Variable | Value | Purpose |
|---|---|---|
| `VOIDSHELL_USER` | `<ssh_username>` | Used by the profile.d script |
| `USER` | `<ssh_username>` | Shell and tool conventions |
| `LOGNAME` | `<ssh_username>` | POSIX login name |

`/etc/profile.d/voidshell-prompt.sh` sets `PS1` to `${VOIDSHELL_USER}@\h:\w\$`
so the prompt displays the SSH username. `whoami` returns `voidshell` because it
looks up UID 1000 in `/etc/passwd`; `$USER` and `$LOGNAME` return the SSH
username.

### Building the workspace image

```bash
docker build -f Dockerfile.workspace \
  -t ghcr.io/stearz/voidshell-workspace:latest .
docker push ghcr.io/stearz/voidshell-workspace:latest
```

---

## Helm values — minimal homelab example

```yaml
# values-homelab.yaml

auth:
  allowedGitHubUsers:
    - stearz             # GitHub username(s) allowed to connect

kubernetes:
  guestNamespace: voidshell-guest   # pre-existing namespace for workspace pods/PVCs
  storageClass: longhorn
  storageSize: 5Gi

workspace:
  shellImage: ghcr.io/stearz/voidshell-workspace:latest
  # shellCommand: omit to use the image CMD (starts a login shell as voidshell/UID 1000)

ssh:
  port: 2222
  hostKeySecret: voidshell-host-key  # K8s Secret in the voidshell namespace

service:
  type: LoadBalancer   # exposes the SSH port externally
  port: 2222

# Target arm64 Raspberry Pi nodes
nodeSelector:
  kubernetes.io/arch: arm64
```

**Prerequisites before deploying:**

```bash
# 1. Create the guest namespace
kubectl create namespace voidshell-guest

# 2. Generate and store the SSH host key
ssh-keygen -t ed25519 -f /tmp/voidshell_host_key -N ""
kubectl create secret generic voidshell-host-key \
  --from-file=host-key=/tmp/voidshell_host_key \
  -n voidshell

# 3. Install the chart
helm upgrade --install voidshell oci://ghcr.io/stearz/charts/voidshell \
  -n voidshell --create-namespace \
  -f values-homelab.yaml
```

---

## Expected session lifecycle

An ordinary session performs only pod creation with an `emptyDir` volume:

```text
ssh devbox@voidshell → CREATE shell-...-ephemeral → attach → DELETE pod
```

A persistent session additionally creates or reuses the stable PVC:

```text
ssh persist.devbox@voidshell → GET/CREATE home-vs-... → CREATE shell-vs-... → attach → DELETE pod
```

Only the persistent-mode PVC remains after disconnect. Ordinary workspace data
disappears with its pod.

---

## Security model and RBAC

### What voidshell can do

| Resource | Namespace | Permissions |
|---|---|---|
| `pods` | `guestNamespace` | get, list, create, delete |
| `pods/log` | `guestNamespace` | get |
| `pods/attach` | `guestNamespace` | create |
| `persistentvolumeclaims` | `guestNamespace` | get, create |

### What voidshell cannot do

- Create or modify namespaces
- Access secrets via the Kubernetes API (host key is mounted as a file)
- Access workloads outside `guestNamespace`
- Run privileged containers (workspace pods run with `RestartPolicy: Never`)
- Delete PVCs (by design — home directories must be deleted manually)

### Authentication flow

1. SSH client offers a public key.
2. voidshell fetches the allowed GitHub user's public keys from
   `https://github.com/<user>.keys` (cached for `keyCacheTTL`, default 5 min).
3. If the offered key matches **exactly one** allowed user: connection proceeds.
4. If the key matches **zero** users: rejected (`unknown key`).
5. If the key matches **multiple** users: rejected (`ambiguous key`).

Removing a key from your GitHub account takes effect within one `keyCacheTTL`.

---

## Manual smoke-test checklist

Run these steps against the homelab cluster after deploying voidshell.

### 0. Verify the pod is running

```bash
kubectl get pod -n voidshell -l app.kubernetes.io/name=voidshell
# Expected: 1/1 Running
```

### 1. Test: ordinary selector creates no PVC

```bash
ssh -p 2222 devbox@<voidshell-service-ip>
# Expected: interactive bash shell appears, prompt shows devbox@<hostname>
# After disconnect: the shell-...-ephemeral pod is gone and no PVC was created.
```

`echo $USER` should return `devbox`, the logical workspace name.

### 2. Test: persistent selector retains a PVC

```bash
ssh -p 2222 persist.devbox@<voidshell-service-ip> 'echo hello > /home/workspace/test.txt'
ssh -p 2222 persist.devbox@<voidshell-service-ip> 'cat /home/workspace/test.txt'
# Expected: hello
```

The persistent pod is deleted after each session, while
`home-vs-stearz-devbox-xxxxxx` remains Bound.

### 3. Test: unknown key → connection rejected

Using a key that is **not** in stearz's GitHub account:

```bash
ssh-keygen -t ed25519 -f /tmp/unknown_key -N ""
ssh -p 2222 -i /tmp/unknown_key devbox@<voidshell-service-ip>
# Expected: Permission denied (publickey)
```

Verify no workspace was created:

```bash
kubectl get pod -n voidshell-guest
# Expected: no new pods
```

### 4. Test: two persistent logical workspaces → separate PVCs

```bash
ssh -p 2222 persist.devbox@<voidshell-service-ip> &
ssh -p 2222 persist.workbox@<voidshell-service-ip> &
```

```bash
kubectl get pvc -n voidshell-guest
# Expected: two PVCs with different workspace IDs:
#   home-vs-stearz-devbox-xxxxxx
#   home-vs-stearz-workbox-yyyyyy
```

### 5. Test: PTY and window resize

```bash
ssh -p 2222 devbox@<voidshell-service-ip>
# Resize the terminal window
# Expected: shell prompt reflows to new width (tput cols should update)
```

---

## Cleanup and restore

### Remove a workspace pod (safe — PVC retained)

```bash
kubectl delete pod shell-vs-stearz-devbox-xxxxxx -n voidshell-guest
# voidshell will recreate the pod on next connection.
```

### Remove a stale workspace (pod + PVC, data lost)

Only do this when you intentionally want to destroy a workspace's home directory.

```bash
# 1. Confirm the workspace ID first
kubectl get pvc -n voidshell-guest -l voidshell.io/workspace-id

# 2. Delete pod (if still running)
kubectl delete pod shell-vs-stearz-devbox-xxxxxx -n voidshell-guest

# 3. Delete PVC — DESTRUCTIVE: home directory data is permanently lost
kubectl delete pvc home-vs-stearz-devbox-xxxxxx -n voidshell-guest
```

### List all workspaces

```bash
kubectl get pvc -n voidshell-guest -l voidshell.io/workspace-id \
  -o custom-columns='WORKSPACE:.metadata.labels.voidshell\.io/workspace-id,PVC:.metadata.name,SIZE:.spec.resources.requests.storage,STATUS:.status.phase'
```

---

## Current scope — what is intentionally excluded

The following are **not** currently supported:

- Shared/multi-user workspace pods
- Management UI or workspace listing API
- Prometheus exporter or metrics
- SFTP / SCP support
- SSH port forwarding or tunnel support
- Automated GitOps deployment pipeline
- Policy engine (time limits, storage quotas beyond PVC size)
- Alternate identity providers (only GitHub public key auth)
