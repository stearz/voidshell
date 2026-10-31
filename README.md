<p align="center">
  <img src="docs/assets/voidshell-wide.png" alt="voidshell — a neon vortex with a terminal and Kubernetes symbol" width="800">
</p>

<h1 align="center">voidshell</h1>

<p align="center">
  Your SSH key. Your workspace. Your Kubernetes cluster.
</p>

<p align="center">
  <a href="#quick-start-for-users">Connect</a> ·
  <a href="#deploy-on-your-cluster">Deploy</a> ·
  <a href="#configuration">Configure</a> ·
  <a href="docs/operations.md">Operations guide</a>
</p>

voidshell turns an SSH connection into a Kubernetes workspace. Sign in with a key
registered on your GitHub account, choose a workspace name, and get a non-root
shell with Homebrew and development tools. Workspaces are disposable by default;
add `persist.` to the SSH username to keep your home directory between sessions.

Built for homelabs and small, trusted groups, with container images for
**linux/amd64 and linux/arm64** and a Helm chart for deployment.

> **Version scope:** This README describes the code in this checkout, including
> the new command, file-transfer, and forwarding support. Published releases may
> have fewer features. Deploy the matching chart and images when testing it.

## At a glance

| Capability | What you get |
|---|---|
| GitHub key authentication | An explicit allow-list of GitHub users; no passwords or workspace user accounts to manage |
| Named workspaces | Separate workspaces per GitHub account and workspace name |
| Disposable or persistent storage | `emptyDir` by default; opt-in PVC-backed home directories |
| Independent sessions | Multiple shells, commands, and transfers share files without sharing a shell process |
| Everyday SSH tools | Interactive terminals, remote commands, SFTP, modern and legacy SCP, and rsync |
| Local port forwarding | Reach development servers inside your own workspace with `ssh -L` |
| Ready-to-use workspace image | Ubuntu, Homebrew, Git, build tools, OpenSSH transfer tools, and rsync; runs as UID 1000 |
| Kubernetes-native deployment | Helm, namespace-scoped RBAC, mounted SSH host key, and proxy NetworkPolicy |

## Quick start for users

Ask your administrator for the server address and SSH port. Your GitHub account
must be on the allow-list, and the public key matching your local SSH key must
be registered on that account. The examples below use port **2222**.

```sh
# Open a disposable workspace called project.
ssh -p 2222 project@voidshell.example.net

# Keep project files across reconnects with a persistent workspace.
ssh -p 2222 persist.project@voidshell.example.net
```

The SSH username selects the workspace; your key identifies your GitHub account.
Two allowed users choosing `project` get different workspaces. One user can
choose several workspace names. Use `-i ~/.ssh/your_key` if your client does not
select the right key automatically.

### Understand what survives

| | `project` | `persist.project` |
|---|---|---|
| Home storage | Temporary `emptyDir` | PersistentVolumeClaim |
| Home and working directory | `/home/workspace` | `/home/workspace` |
| While sessions overlap | Files shared within this workspace | Files shared within this workspace |
| After the last active channel closes | Pod and files are removed | Pod is removed; home volume remains |
| Container changes outside the home volume | Discarded | Discarded |

These selectors use different pods. Switching to `persist.project` does **not**
copy files from `project`. Homebrew packages are installed outside the home
volume, so they disappear when either pod is deleted. Bake recurring tools into
a custom image.

The logical workspace name appears in the prompt, `$USER`, and `$LOGNAME`.
`whoami` returns the pre-baked Linux user `voidshell` (UID 1000); the SSH username
does not create a Linux account.

### Run commands and transfer files

```sh
# Commands preserve their exit status; stderr is separate without a PTY.
ssh -p 2222 persist.project@voidshell.example.net 'git --version'

# Upload files or open an interactive SFTP session.
scp -P 2222 ./file.txt persist.project@voidshell.example.net:/home/workspace/
sftp -P 2222 persist.project@voidshell.example.net

# Synchronize a local source directory.
rsync -av -e 'ssh -p 2222' ./src/ \
  persist.project@voidshell.example.net:/home/workspace/src/
```

Use `scp -O` if you need the legacy SCP protocol. The supplied workspace image
includes the required server-side binaries for SFTP, SCP, and rsync.

### Preview a development server

```sh
ssh -p 2222 -L 8080:localhost:3000 persist.project@voidshell.example.net
# Inside this shell, start your application on 127.0.0.1:3000.
# Open http://localhost:8080 on your own computer.
```

Keep the shell open while using the application. Forwarding reaches the selected
workspace's loopback through the selected backend, including servers that do not
listen on the pod IP. Accepted destinations are `localhost`, `127.0.0.1`, and
`::1`. Kubernetes mode uses the kubelet's loopback family; SSH mode connects to
`127.0.0.1` inside the workspace.

There is **no idle grace period**. The last active shell, command, SFTP session,
or forwarding channel closing deletes the pod. An idle SSH transport or
`ssh -N -L ...` does not retain a workspace. Forward-only connections provision
lazily when traffic opens a forwarding channel.

## Deploy on your cluster

### What you need

- A Kubernetes cluster and permission to install the chart's namespaced resources.
- Helm and `kubectl`, configured for the target cluster.
- A guest namespace for workspace pods and optional PVCs.
- A persistent SSH host key, stored as a Secret in the proxy namespace.
- Network access from the proxy to GitHub, DNS, and the Kubernetes API.
- A default StorageClass, or an explicit class such as Longhorn, if you want
  persistent workspaces. Disposable workspaces do not need a StorageClass.
- A way to expose TCP port 2222: for example a LoadBalancer, NodePort, or your
  platform's TCP routing. A regular HTTP Ingress is not sufficient.

Run **one proxy instance per guest namespace**. Workspace lifecycle coordination
is in memory. The chart enforces one replica and uses `Recreate` upgrades, which
interrupt active SSH connections.

### 1. Prepare namespaces and the host key

```sh
kubectl create namespace voidshell
kubectl create namespace voidshell-guest

key_dir=$(mktemp -d)
ssh-keygen -t ed25519 -f "$key_dir/host-key" -N ''
kubectl create secret generic voidshell-host-key \
  --from-file=host-key="$key_dir/host-key" \
  -n voidshell
```

Keep the host key stable across upgrades, protect its private material, and back
up the Secret according to your platform's policy. Handle the temporary local
key files securely after importing them. Share the host-key fingerprint with
users so they can verify their first connection.

### 2. Create your values file

Save this as `values-homelab.yaml`. Replace `YOUR_GITHUB_USERNAME` and the example
API-server address before installing.

```yaml
image:
  tag: main # Development image; pin a matching revision for repeatable deployments.

auth:
  allowedGitHubUsers:
    - YOUR_GITHUB_USERNAME

kubernetes:
  guestNamespace: voidshell-guest
  storageClass: "" # Cluster default; only used for persist.<workspace>.
  storageSize: 5Gi

workspace:
  image: ghcr.io/stearz/voidshell-workspace:main
  shellCommand: [/bin/bash, -l]

ssh:
  hostKeySecret: voidshell-host-key

service:
  type: LoadBalancer
  port: 2222

networkPolicy:
  enabled: true
  egressPrivateExceptions:
    - 192.168.1.50/32 # EXAMPLE: replace with your reachable API-server IP/CIDR.
```

The proxy NetworkPolicy blocks private destination ranges by default. Allow the
Kubernetes API endpoint actually used by your cluster; the example IP is not
universal. Policy enforcement requires a compatible CNI. Workspace forwarding
uses the API by default. The optional SSH backend needs direct pod networking;
see [direct SSH requirements](docs/adr/005-direct-ssh-backend.md).

`nodeSelector` in this chart schedules the **proxy**. Workspace pod scheduling
follows the cluster defaults; it does not inherit that setting.

### 3. Install and connect

To deploy the chart from **this checkout**:

```sh
helm upgrade --install voidshell ./charts/voidshell \
  -n voidshell \
  -f values-homelab.yaml

kubectl get pods,svc -n voidshell
ssh -p 2222 project@<service-address>
```

Local source changes must first be built into images your cluster can pull.
Override `image.tag` and `workspace.image` with those image references. Pin both
to known revisions for reproducible deployments; `:main` is a moving tag.

For a **published version**, use the OCI chart instead and select a chart version
from the [releases](https://github.com/stearz/voidshell/releases):

```sh
helm upgrade --install voidshell oci://ghcr.io/stearz/charts/voidshell \
  --version <chart-version> \
  -n voidshell \
  -f values-homelab.yaml
```

For a released deployment, remove `image.tag: main` from your values file so the
chart selects its release image, or explicitly pin the matching release tag.
Check that version's feature set before using the examples above. The new exec
and forwarding support requires the chart's `pods/exec` and `pods/portforward`
permissions and an updated workspace image for file-transfer binaries.

## Operating model and security

```text
SSH client → voidshell → Kubernetes API → workspace pod
                 │
                 └─ validates the offered key against allowed GitHub users
```

The authenticated GitHub account and workspace name form the workspace identity.
The SSH username selects a workspace; it is not an authentication factor.

| Area | Operator responsibility and behavior |
|---|---|
| Authentication | An empty allow-list denies everyone. A key must match exactly one allowed GitHub account. Expired key-cache entries fail closed if GitHub cannot be reached. |
| Key revocation | GitHub key removals affect new authentications after the cache TTL, normally 5 minutes. Existing sessions are not automatically revoked. |
| Kubernetes permissions | The proxy Role is confined to the guest namespace: pods get/list/create/delete, pod logs get, and PVC get/create. Kubernetes mode adds exec/port-forward create; SSH mode adds Secret get/create. The listener host key is mounted as a file. |
| Workspace execution | Processes run as UID 1000 with privilege escalation disabled and all capabilities dropped. `fsGroup: 1000` makes supported home volumes writable. |
| Network isolation | The chart protects proxy ingress/egress and, in SSH mode, workspace SSH ingress. Workspace API tokens are not mounted. Supply workspace egress policies and review other additive network policies. |
| Resource control | Chart resource requests and limits apply to the proxy. Use namespace ResourceQuota and LimitRange policies to constrain workspace CPU, memory, storage, and object counts. |
| Data retention | PVCs are never automatically deleted, including after removing a user. Plan backups, inventory, and deliberate cleanup. |
| Shutdown and recovery | Graceful shutdown attempts pod cleanup. Crashes can leave orphaned pods; operators must inspect and remove them. |

voidshell is intended for trusted homelab users. It is not a hardened sandbox for
hostile tenants; pods share the cluster's kernel and platform security model.

For upgrades from the previous attach-based session model, inspect and remove
idle workspace pods before reconnecting; retain their PVCs. See the
[operations guide](docs/operations.md) for smoke checks, cleanup, and recovery,
and [security decisions](docs/security-decisions.md) for accepted limitations.

### Current limits

Remote forwarding (`ssh -R`), arbitrary forwarding destinations,
agent forwarding, X11 forwarding, and client environment variables
beyond PTY `TERM` are unsupported. Signal forwarding is available only with the
SSH backend. VS Code Remote SSH compatibility has not been
validated. There is no management UI, automatic orphan cleanup, idle TTL, or
multi-replica coordination. See [SSH compatibility](docs/openssh-parity-plan.md).

## Configuration

For Helm deployments, use the [chart values](charts/voidshell/values.yaml) and
[chart reference](charts/voidshell/README.md). `ssh.hostKeySecretKey` is a chart
setting selecting the Secret data key; its default is `host-key`.

For running the binary directly, start with
[the example config](configs/voidshell.example.yaml). YAML is loaded over defaults;
environment variables then override individual fields.

```sh
./bin/voidshell -config /path/to/voidshell.yaml
# Alternatively, set VOIDSHELL_CONFIG to that path.
```

| Environment variable | Config field | Binary default |
|---|---|---|
| `VOIDSHELL_SSH_PORT` | `ssh.port` | `2222` |
| `VOIDSHELL_SSH_HOST_KEY_PATH` | `ssh.hostKeyPath` | Empty |
| `VOIDSHELL_SSH_HOST_KEY_SECRET` | `ssh.hostKeySecret` | Empty |
| `VOIDSHELL_AUTH_ALLOWED_USERS` | `auth.allowedGitHubUsers` | Empty, denies everyone; comma-separated override |
| `VOIDSHELL_AUTH_KEY_CACHE_TTL` | `auth.keyCacheTTL` | `5m` |
| `VOIDSHELL_K8S_GUEST_NAMESPACE` | `kubernetes.guestNamespace` | `voidshell-workspaces` (chart: `voidshell-guest`) |
| `VOIDSHELL_K8S_STORAGE_CLASS` | `kubernetes.storageClass` | Empty, uses cluster default |
| `VOIDSHELL_K8S_STORAGE_SIZE` | `kubernetes.storageSize` | `5Gi` |
| `VOIDSHELL_WORKSPACE_BACKEND` | `workspace.backend` | `kubernetes`; optionally `ssh` |
| `VOIDSHELL_WORKSPACE_IMAGE` | `workspace.image` | `ghcr.io/stearz/voidshell-workspace:main` |
| `VOIDSHELL_WORKSPACE_SHELL_COMMAND` | `workspace.shellCommand` | `/bin/bash,-l`; comma-separated override |

The binary uses in-cluster Kubernetes credentials, falling back to the default
kubeconfig for local development. The Helm deployment reads the host key from a
mounted file. Only the direct SSH backend needs Secret get/create permission
in the guest namespace.

## Direct SSH backend

Set `workspace.backend: ssh` in Helm values or binary YAML (environment override:
`VOIDSHELL_WORKSPACE_BACKEND=ssh`). Use a workspace image built from this revision;
older images have no SSH startup script. Kubernetes remains the default.

SSH mode routes shell, SFTP/SCP, and local-forwarding data directly to the selected
pod's SSH daemon on TCP 2222. The API handles lifecycle and per-pod credentials.
The proxy pins a unique host key and authenticates with a separate key that is
never mounted into the workspace. Keys rotate when the pod is recreated; Secrets
are garbage-collected with it. The chart supplies the necessary RBAC and selected
network rules; your CNI must enforce NetworkPolicy and route proxy-to-pod traffic.
Inspect and remove old workspace pods when switching modes, preserving PVCs.

The official daemon runs as UID 1000 with no capabilities. Its passwd home is
`/home/workspace`, and its passwd shell handles shell/exec requests;
`workspace.shellCommand` configures only Kubernetes mode. A direct session channel
holds a workspace lease from channel open until close. Unknown environment,
agent, X11, and remote-forwarding requests remain rejected.

See [the image, security, and networking contract](docs/adr/005-direct-ssh-backend.md)
and [benchmark procedure and results](docs/backend-benchmarks.md).

## Customize the workspace image

The supplied [Dockerfile.workspace](Dockerfile.workspace) provides Ubuntu,
Homebrew at `/home/linuxbrew/.linuxbrew`, a pre-baked non-root `voidshell` user,
and the transfer tools. Homebrew is writable through the `brew` group and is
available to login shells and remote commands.

```sh
docker build -f Dockerfile.workspace -t <registry>/voidshell-workspace:<tag> .
```

Publish your image to a registry your cluster can access, then set
`workspace.image` to that reference. For private registries, configure workspace
image-pull access in the guest namespace; the chart's `imagePullSecrets` setting
applies to the proxy only.

For the default Kubernetes backend, custom images need `/bin/sh`, `sleep`, `env`,
and the configured shell. The proxy
runs a keepalive process as PID 1 and starts each session through Kubernetes
exec; custom image entrypoints are bypassed. An empty `shellCommand` falls back
to `/bin/bash -l`. SFTP needs OpenSSH `sftp-server` at a supported path; legacy
SCP and rsync need their respective binaries. See
[the session architecture](docs/adr/004-independent-sessions-and-workspace-lifecycle.md).

## Development and preview builds

You need **Go 1.26+** and `make` for local development.

```sh
go mod download
make test
make vet
make build
./bin/voidshell --help
```

CI tests pull requests, lints Helm, and scans both images. Successful pushes to
`main` publish both images for amd64 and arm64 with `:main` and commit-SHA tags.
PR builds do not publish images. You can test a main-branch commit without
creating a versioned release by deploying the local chart with both SHA tags:

```sh
helm upgrade --install voidshell ./charts/voidshell \
  -n voidshell \
  -f values-homelab.yaml \
  --set-string image.tag=<commit-sha> \
  --set-string workspace.image=ghcr.io/stearz/voidshell-workspace:<commit-sha>
```

Release-please accumulates conventional commits into a release PR. Merging that
PR creates the GitHub Release and tag; the release workflow then publishes the
versioned proxy image and OCI Helm chart. `feat:` adds a minor version, `fix:` a
patch, and breaking changes a major version. See
[the workflows](.github/workflows) for the exact build and release configuration.

`VERSION` contains the release version as a single plain-text line for scripts
and packaging tools. Release-please uses its `simple` strategy with
`version-file: VERSION` to update it alongside the changelog, release manifest,
and Helm chart metadata in each release PR. The generic updater requires inline
annotations and would leave this plain version file unchanged. The application
does not currently read `VERSION` at runtime.

## Further reading

- [Operations guide](docs/operations.md) — deployment, smoke checks, cleanup, and recovery.
- [Chart reference](charts/voidshell/README.md) — Helm values and RBAC.
- [Project knowledge](docs/knowledge/index.md) — identity, storage, architecture, and security decisions.
- [SSH compatibility](docs/openssh-parity-plan.md) — implemented features and deferred work.
- [MIT license](LICENSE).
