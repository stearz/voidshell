# voidshell

A small SSH-to-Kubernetes workspace proxy for homelabs.  
Replaces the ContainerSSH idea with explicit arm64 support.

## What it does

voidshell accepts SSH connections, authenticates the connecting user against a
GitHub-user allow-list, and provisions an ephemeral Kubernetes workspace pod for
them. The workspace identity is the tuple `(github_username, ssh_username)`,
which allows one GitHub account to maintain multiple independent workspaces.

See [docs/adr/001-identity-and-naming.md](docs/adr/001-identity-and-naming.md)
for the full identity model and Kubernetes object naming rules.

## Status

Scaffold / early development. SSH auth and Kubernetes provisioning are not yet
implemented.

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
| `VOIDSHELL_WORKSPACE_SHELL_IMAGE` | `workspace.shellImage`              | `ubuntu:22.04`           |
| `VOIDSHELL_WORKSPACE_SHELL_COMMAND`| `workspace.shellCommand` (CSV)     | `/bin/bash`              |

## Project layout

```
cmd/voidshell/       main entrypoint
internal/config/     configuration loading and types
configs/             example config file
docs/adr/            architecture decision records
```

## Author(s)

- Stephan Schwarz ([@stearz](https://github.com/stearz))
