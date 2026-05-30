# voidshell — Operations Guide

This document covers the identity model, object naming, Helm values, expected
lifecycle, security/RBAC assumptions, and a manual smoke-test checklist.

---

## Identity model

A voidshell workspace is uniquely identified by the tuple:

```
(github_username, ssh_username)
```

- **`github_username`** — the authenticated GitHub account whose public keys were
  accepted during the SSH handshake. This is the security boundary.
- **`ssh_username`** — the username the client used in the SSH connection (e.g.
  `ssh devbox@voidshell.homelab`). This acts as a workspace *selector*, not an
  auth factor. One GitHub account can have multiple independent workspaces by
  using different SSH usernames.

### Key properties

| Property | Description |
|---|---|
| Two users, same SSH username | Different workspaces (GitHub user is the discriminator) |
| Same GitHub user, different SSH usernames | Different workspaces (SSH username is the workspace label) |
| Same GitHub user, same SSH username | Same workspace — PVC is reused across reconnects |
| Unknown SSH public key | Connection rejected before any workspace is touched |

---

## Kubernetes object naming

All workspace objects use a deterministic, Kubernetes-safe name derived from the
identity tuple:

```
workspace_id = vs-<normalized-github>-<normalized-ssh>-<hash6>
pod_name     = shell-<workspace_id>
pvc_name     = home-<workspace_id>
```

**Normalization rules:**
- Lowercased, non-alphanumeric characters replaced with `-`, leading/trailing
  hyphens stripped, truncated to 26 characters per segment.
- A 6-character SHA-256 hash of the raw (un-normalized) tuple ensures uniqueness
  even if normalized segments collide.

**Example:**

| GitHub user | SSH username | workspace_id |
|---|---|---|
| `stearz` | `devbox` | `vs-stearz-devbox-xxxxxx` |
| `stearz` | `workbox` | `vs-stearz-workbox-yyyyyy` |
| `octocat` | `devbox` | `vs-octocat-devbox-zzzzzz` |

The full workspace ID is always ≤ 63 characters (RFC 1123 DNS label compliant).

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

```
Client                          voidshell                        Kubernetes
  │                                  │                                │
  │── ssh devbox@voidshell ─────────>│                                │
  │   (offers SSH public key)        │── verify key against GitHub ──>│
  │                                  │<─ key matches stearz ──────────│
  │                                  │                                │
  │                                  │── GET PVC home-vs-... ────────>│
  │                                  │   (not found → CREATE) ────────│
  │                                  │── GET pod shell-vs-... ───────>│
  │                                  │   (not found → CREATE) ────────│
  │                                  │── wait for pod Running ───────>│
  │                                  │<─ pod Running ─────────────────│
  │                                  │                                │
  │<── PTY attached ────────────────>│── pods/attach SPDY stream ────>│
  │  (interactive shell)             │                                │
  │                                  │                                │
  │── disconnect ───────────────────>│                                │
  │                                  │── DELETE pod shell-vs-... ────>│
  │                                  │   PVC home-vs-... RETAINED ────│
```

**PVC retention is intentional.** The home directory persists across reconnects.
When the same `(github_user, ssh_user)` pair reconnects, the existing PVC is
reused and data is preserved.

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

### 1. Test: allowed key → shell reached as correct user

```bash
ssh -p 2222 devbox@<voidshell-service-ip>
# Expected: interactive bash shell appears, prompt shows devbox@<hostname>
```

Inside the shell, verify identity and Homebrew:

```bash
whoami
# Expected: voidshell  (pre-baked Linux user, UID 1000)

echo $USER
# Expected: devbox  (SSH username injected by voidshell)

brew --version
# Expected: Homebrew <version>
```

Verify the workspace objects were created:

```bash
kubectl get pod -n voidshell-guest
# Expected: shell-vs-stearz-devbox-xxxxxx  Running

kubectl get pvc -n voidshell-guest
# Expected: home-vs-stearz-devbox-xxxxxx  Bound

kubectl logs -n voidshell <voidshell-pod> | grep "ssh: connection"
# Expected: github_user=stearz ssh_user=devbox
```

After disconnecting:

```bash
kubectl get pod -n voidshell-guest
# Expected: pod is gone

kubectl get pvc -n voidshell-guest
# Expected: PVC still present (intentionally retained)
```

### 2. Test: unknown key → connection rejected

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

### 3. Test: same GitHub user, two SSH usernames → separate workspaces

```bash
# First workspace
ssh -p 2222 devbox@<voidshell-service-ip> &
# Second workspace
ssh -p 2222 workbox@<voidshell-service-ip> &
```

```bash
kubectl get pvc -n voidshell-guest
# Expected: two PVCs with different workspace IDs:
#   home-vs-stearz-devbox-xxxxxx
#   home-vs-stearz-workbox-yyyyyy
```

### 4. Test: reconnect reuses PVC

```bash
# Create a file in the workspace
ssh -p 2222 devbox@<voidshell-service-ip> 'echo hello > ~/test.txt'
# Reconnect
ssh -p 2222 devbox@<voidshell-service-ip> 'cat ~/test.txt'
# Expected: hello
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
