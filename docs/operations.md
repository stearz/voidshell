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
| `USER` | `voidshell` | Linux account for tool compatibility |
| `LOGNAME` | `voidshell` | Linux login name |

`/etc/profile.d/voidshell-prompt.sh` sets `PS1` to `${VOIDSHELL_USER}@\h:\w\$`
so the prompt displays the SSH username. `whoami` returns `voidshell` because it
looks up UID 1000 in `/etc/passwd`; `$USER` and `$LOGNAME` also return
`voidshell`. Keep these variables aligned with the Linux account: Homebrew
looks up `$USER` when extracting packages. `VOIDSHELL_USER` carries the SSH
username for display only.

The image transfers Homebrew repository ownership from its build user to
`voidshell`, allowing Git operations such as `brew update` without trust
exceptions. These image and pod-environment fixes apply to newly created
workspaces after updating both server and workspace images. An existing
workspace can use `export USER="$(id -un)" LOGNAME="$(id -un)"` and
`git config --global --add safe.directory /home/linuxbrew/.linuxbrew/Homebrew`
as a temporary workaround. The exports apply only to the current shell.

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
  image: ghcr.io/stearz/voidshell-workspace:latest
  shellCommand: [/bin/bash, -l] # independent login shell per session

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

```text
first active channel → CREATE pod → wait for container readiness → exec/forward
additional channels  → independent processes/streams in the same pod
last channel closes  → DELETE pod → wait for deletion
```

`devbox` uses `emptyDir` without a PVC; `persist.devbox` creates/reuses the stable
PVC. These selectors use separate pods. Each shell is an independent exec
process, with `HOME` and working directory `/home/workspace`. Only a persistent
home survives deletion. Installed Homebrew packages are outside the home volume.

An idle SSH transport does not retain a pod. Forward-only connections provision
on the first incoming forwarding channel and release on its completion. Keep a
shell session open while using a development server. There is no idle TTL.

Run exactly one proxy instance per guest namespace. The Helm chart enforces a
single replica with `Recreate` upgrades and allows 45 seconds for graceful
shutdown. During an upgrade, active SSH connections close and pods are cleaned
up. Abrupt crashes can leave orphaned pods; inspect and remove those manually.
When migrating from the old attach-based model, inspect and remove idle workspace
pods before reconnecting. Do not delete their PVCs.

Custom images must provide `/bin/sh`, `sleep`, `env`, and the configured session
shell; custom entrypoints are bypassed. The image's `sftp-server`, `scp`, and
`rsync` binaries are required for those features. Rebuild the supplied image to
include the new binaries. Volumes must support `fsGroup: 1000` ownership.

See [ADR 004](adr/004-independent-sessions-and-workspace-lifecycle.md) for the
complete lifetime, cancellation, and deployment contract.

## Security model and RBAC

### What voidshell can do

| Resource | Namespace | Permissions |
|---|---|---|
| `pods` | `guestNamespace` | get, list, create, delete |
| `pods/log` | `guestNamespace` | get |
| `pods/exec` | `guestNamespace` | create |
| `pods/portforward` | `guestNamespace` | create |
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

Use two interactive sessions for the persistent smoke test:

```bash
ssh -p 2222 persist.devbox@<voidshell-service-ip>
# Inside the shell: echo hello > /home/workspace/test.txt ; exit
ssh -p 2222 persist.devbox@<voidshell-service-ip>
# Inside the shell: cat /home/workspace/test.txt ; exit
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

### 6. Test: independent sessions share files

Open two terminals to `persist.devbox`. Create a file in `/home/workspace` in
one and read it in the other. Exit the first terminal. The second must continue
working, with the same pod. Exit the second: the pod must disappear and its PVC
must remain. Repeat with `devbox`; its files must disappear after both exit.

### 7. Test: commands, exit codes, and transfers

```bash
ssh -p 2222 persist.devbox@<host> 'printf out; printf err >&2; exit 7'
# Expect separate stdout/stderr and local SSH exit code 7.
scp -P 2222 ./file.txt persist.devbox@<host>:/home/workspace/
sftp -P 2222 persist.devbox@<host>
rsync -av -e 'ssh -p 2222' ./src/ persist.devbox@<host>:/home/workspace/src/
```

Confirm uploaded files survive a persistent reconnect. SFTP stdout must have no
shell prompt or banner. Also test `scp -O` for legacy SCP if you use it.

### 8. Test: workspace-local development server

```bash
ssh -p 2222 -L 8080:localhost:3000 persist.devbox@<host>
# Inside the workspace, start an HTTP server listening on 127.0.0.1:3000.
# From your own machine, visit http://localhost:8080.
```

A loopback-only server must be reachable. Forwarding to an arbitrary cluster IP
or hostname must be rejected. Close a transfer or forwarding connection while a
shell remains active: the shell and pod must remain usable. Close the final
channel: the pod must be deleted.

### 9. Test: failed setup and disconnect

Use an invalid workspace image in a test deployment. Connecting must fail and
its partially created pod must be removed. Disconnect during pod startup and
verify cleanup as well. Restore the image, reconnect, and confirm a fresh pod
can start. Stop the proxy gracefully with active sessions and verify cleanup;
test abrupt crash recovery separately, since orphan cleanup is manual.

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
- Remote SSH forwarding (`-R`) and forwarding to arbitrary destinations
- Automated GitOps deployment pipeline
- Policy engine (time limits, storage quotas beyond PVC size)
- Alternate identity providers (only GitHub public key auth)

## Image publication and retention

CI tests and lints pushes to `main` and pull requests. Image builds and scans
skip changes limited to Markdown, `docs/`, or `LICENSE`. Pull requests build
without publishing. Main publishes both `voidshell` and `voidshell-workspace`
with `main` and full commit-SHA tags. Published GitHub releases publish both
images with the release tag and `latest`.

The daily cleanup runs at 04:23 UTC (also available via workflow_dispatch).
It removes SHA-only development versions and orphaned untagged manifests older
than 24 hours. The current `main`, `latest`, release tags, and other non-SHA
tags are retained indefinitely, including all recursively referenced platform
and attestation manifests. Recent versions and their dependencies are also
protected. Resolution errors stop cleanup before deletion. Helm packages are
outside the cleanup scope.

Actual retention is roughly 24–48 hours, potentially longer if GitHub delays or
skips a scheduled run. Historical SHA references can stop pulling after cleanup;
use release tags for durable deployments. The most recently published `main`
remains pullable even without new builds. Publishing and cleanup share a
concurrency group to avoid deleting manifests during publication.

The repository's GITHUB_TOKEN needs admin access to both container packages
for deletion, in addition to the workflow's `packages: write` permission.
Packages created by this repository's workflows normally grant that access;
for existing packages verify their Actions access in package settings.

## Direct SSH backend checks

Use `workspace.backend: ssh` with the updated workspace image. Remove idle old
workspace pods before changing backends; keep PVCs. Review
[ADR 005](adr/005-direct-ssh-backend.md) for the image and network contracts.
In SSH mode the proxy needs direct TCP 2222 reachability to pod IPs, guest-namespace
Secret get/create, and no exec/port-forward API permissions. Confirm your CNI
enforces the chart's selected workspace ingress; broader additive policies can
allow additional sources.

Repeat the shell, resize, SFTP/SCP, and forwarding smoke checks above. Check that
workspace pods run `/usr/local/bin/voidshell-sshd`, become ready on port 2222, and
have `automountServiceAccountToken: false`. Each pod references one immutable,
pod-owned `workspace-ssh-*` Secret. Its volume projection must include only
`host-key` and `authorized_keys`, never `client-key`. The last channel must delete
the pod; its Secret should subsequently disappear through garbage collection.
Reconnecting must create a new Secret and host key, preserving only PVC-backed
home data. Never print private key data while inspecting these resources.

To rotate internal keys, close all workspace channels and recreate the pod. For
emergency revocation, terminate active proxy transports and the affected pod;
changing a Secret does not invalidate established SSH connections. A mismatched
host key or incompatible old pod fails closed and appears in the proxy log.
An orphan pod after a crash follows the existing manual recovery procedure.

[Backend benchmarks](backend-benchmarks.md) provide the disposable local-cluster
comparison and the Docker/OpenSSH integration command.
