# voidshell

A small SSH-to-Kubernetes workspace proxy for homelabs.  
Replaces the ContainerSSH idea with explicit arm64 support.

## What it does

voidshell accepts SSH connections, authenticates the connecting user against a
GitHub-user allow-list, and provisions an ephemeral Kubernetes workspace pod for
them. The workspace identity is the tuple `(github_username, ssh_username)`,
which allows one GitHub account to maintain multiple independent workspaces.

Inside the workspace pod you run as the SSH username you connected with — not as
`root` or a generic user. See [Workspace image](#workspace-image) below and
[docs/adr/001-identity-and-naming.md](docs/adr/001-identity-and-naming.md) for
the full identity model and Kubernetes object naming rules.

## Status

Phase 1 complete: SSH auth with GitHub public key validation, Kubernetes workspace lifecycle (PVC + pod), interactive shell sessions with PTY support, and a Helm chart for GitOps deployments.

## Prerequisites

- Go 1.25+
- `make`

## Local development

```sh
# Download dependencies
go mod download

# Run tests
make test

# Vet
make vet

# Build the binary
make build
./bin/voidshell --help
```

## Configuration

voidshell is configured with a YAML file and/or environment variables. Copy
`configs/voidshell.example.yaml` as a starting point:

```sh
cp configs/voidshell.example.yaml voidshell.yaml
# edit voidshell.yaml …
./bin/voidshell -config voidshell.yaml
```

The config file path can also be provided via the `VOIDSHELL_CONFIG` environment
variable.

### Environment variable overrides

Every config field can be overridden individually, which is useful for injecting
Kubernetes Secrets without rewriting the whole config file.

| Environment variable              | Config field                        | Default                  |
|-----------------------------------|-------------------------------------|--------------------------|
| `VOIDSHELL_SSH_PORT`              | `ssh.port`                          | `2222`                   |
| `VOIDSHELL_SSH_HOST_KEY_PATH`     | `ssh.hostKeyPath`                   | `""`                     |
| `VOIDSHELL_SSH_HOST_KEY_SECRET`   | `ssh.hostKeySecret`                 | `""`                     |
| `VOIDSHELL_AUTH_ALLOWED_USERS`    | `auth.allowedGitHubUsers` (CSV)     | `[]`                     |
| `VOIDSHELL_AUTH_KEY_CACHE_TTL`    | `auth.keyCacheTTL`                  | `5m`                     |
| `VOIDSHELL_K8S_GUEST_NAMESPACE`   | `kubernetes.guestNamespace`         | `voidshell-workspaces`   |
| `VOIDSHELL_K8S_STORAGE_CLASS`     | `kubernetes.storageClass`           | `standard`               |
| `VOIDSHELL_K8S_STORAGE_SIZE`      | `kubernetes.storageSize`            | `5Gi`                    |
| `VOIDSHELL_WORKSPACE_SHELL_IMAGE` | `workspace.shellImage`              | `ubuntu:26.04`           |
| `VOIDSHELL_WORKSPACE_SHELL_COMMAND`| `workspace.shellCommand` (CSV)     | `/bin/bash`              |

## Workspace image

voidshell ships a purpose-built workspace image (`Dockerfile.workspace`) that
provides:

- **Homebrew** pre-installed at `/home/linuxbrew/.linuxbrew`, writable by
  workspace users via the `brew` group.
- **Non-root execution** — the container starts directly as the pre-baked
  `voidshell` user (UID 1000); it never runs as root. voidshell injects the
  SSH username as `VOIDSHELL_USER`, `USER`, and `LOGNAME` env vars, and a
  `/etc/profile.d` script sets the shell prompt to the SSH username so you
  see `devbox@hostname` instead of `voidshell@hostname`.

### How user identity flows into the container

```
ssh devbox@voidshell.homelab
     │
     ▼
voidshell authenticates via GitHub keys
  → github_user = stearz   (who you are)
  → ssh_user    = devbox   (what you typed before @)
     │
     ▼
Pod created with:
  env VOIDSHELL_USER=devbox  USER=devbox  LOGNAME=devbox
  securityContext: runAsUser=1000, runAsNonRoot=true
     │
     ▼
Container starts as pre-baked user "voidshell" (UID 1000) — never root
/etc/profile.d/voidshell-prompt.sh sets PS1 using $VOIDSHELL_USER
     │
     ▼
Prompt shows:  devbox@hostname:~$
whoami returns: voidshell  (UID 1000 in /etc/passwd)
$USER returns:  devbox
```

### Building and using the workspace image

```sh
docker build -f Dockerfile.workspace -t ghcr.io/stearz/voidshell-workspace:latest .
docker push ghcr.io/stearz/voidshell-workspace:latest
```

In your voidshell config, point `shellImage` at the built image and leave
`shellCommand` unset (or omit it) so the image's `ENTRYPOINT` runs:

```yaml
workspace:
  shellImage: ghcr.io/stearz/voidshell-workspace:latest
  # shellCommand: omit to use the image CMD (recommended)
```

A plain base image (`ubuntu:26.04`) still works if you set `shellCommand: [/bin/bash]` —
you will just land as `root` without Homebrew.

## Project layout

```
cmd/voidshell/       main entrypoint
internal/config/     configuration loading and types
configs/             example config file
docs/adr/            architecture decision records
```

## Releasing

Releases are managed by [release-please](https://github.com/googleapis/release-please). Merging conventional commits to `main` accumulates release notes; when a release PR created by the bot is merged, it:

1. Creates a GitHub Release and semver tag (e.g. `v0.2.0`).
2. Triggers the release workflow which publishes:
   - A multi-arch container image to `ghcr.io/stearz/voidshell:<tag>`
   - A Helm chart OCI artifact to `oci://ghcr.io/stearz/charts/voidshell`

### Commit convention

| Prefix | Effect |
|---|---|
| `feat:` | Bumps minor version |
| `fix:` | Bumps patch version |
| `feat!:` / `fix!:` / `BREAKING CHANGE:` | Bumps major version |
| `chore:`, `docs:`, `test:`, `ci:` | No release triggered |

### Install released chart

```
helm install voidshell oci://ghcr.io/stearz/charts/voidshell \
  --version 0.1.0 \
  -n voidshell --create-namespace \
  -f values-homelab.yaml
```

## Author(s)

- Stephan Schwarz ([@stearz](https://github.com/stearz))
