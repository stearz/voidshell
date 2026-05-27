# ADR 002: Workspace Image and In-Container User Identity

**Status:** Accepted  
**Date:** 2026-05-27

---

## Context

The initial workspace pod ran whatever image was configured (`shellImage`) with a
fixed shell command (`shellCommand`). This meant users always landed as `root` (or
the image's default user), regardless of who they were. There was no Homebrew or
other tooling pre-installed.

Two problems to solve:

1. **User experience** — running as `root` inside every workspace is surprising and
   unnecessary. The SSH username is already known at pod-creation time and should
   be reflected inside the container.
2. **Tooling** — a bare `ubuntu` image provides no package manager beyond `apt`.
   Users expect to be able to install tools without root friction.

## Decision

### Workspace image (`Dockerfile.workspace`)

A purpose-built workspace image is provided alongside the voidshell server image.
It is not built automatically by the release workflow (which builds the server
binary image); operators build and push it separately.

The image:

- Bases on `ubuntu:26.04` (current LTS).
- Pre-installs Homebrew at `/home/linuxbrew/.linuxbrew` using a dedicated
  `linuxbrew` build user (UID/GID 900). The prefix is owned by the `brew` group
  with group-write + setgid permissions, so any user added to that group can
  install packages.
- Pre-bakes a `voidshell` user (UID 1000, member of the `brew` group) so the
  container starts directly as a non-root user.
- Uses `CMD ["/bin/bash", "-l"]` with no `ENTRYPOINT`; user setup happens at
  image build time, not at container start.

### Env-var bridge: `VOIDSHELL_USER`

voidshell injects `VOIDSHELL_USER=<ssh_username>` into the pod container spec at
creation time. This is the only change to the server side — no new config field,
no API surface.

`ssh_username` (the text before `@` in the SSH command) was already captured in
`workspace.Identity.SSHUser`; the env var is simply the transport that carries it
from the Go process into the container's runtime environment.

### Workspace user (`voidshell`, UID 1000)

A `voidshell` user is pre-baked into the image at UID 1000, a member of the
`brew` group. The image has no `ENTRYPOINT`; it uses `CMD ["/bin/bash", "-l"]`
so the container starts a login shell directly as that user.

### Pod securityContext

voidshell sets `securityContext.runAsUser: 1000` and `runAsNonRoot: true` on the
workspace container. The container therefore never runs as root at any point
during its lifecycle.

### Prompt identity (`/etc/profile.d/voidshell-prompt.sh`)

Alongside `VOIDSHELL_USER`, voidshell also injects `USER` and `LOGNAME` set to
the SSH username. A `/etc/profile.d/voidshell-prompt.sh` script sets `PS1` to
`${VOIDSHELL_USER}@\h:\w\$`, so the connecting user sees their SSH username in
the prompt. `whoami` still returns `voidshell` (it resolves UID 1000 from
`/etc/passwd`), but `$USER` and `$LOGNAME` return the SSH username.

Homebrew is wired into all login shells via `/etc/profile.d/homebrew.sh`, which
is baked into the image at build time.

### UID strategy

All workspace users share UID 1000 inside the container. The username is a
display name; the UID determines file ownership. Because each workspace pod uses
a dedicated PVC, there is no cross-user collision risk — the UID is always "this
user" within their own PVC.

## Consequences

- Operators must build and publish the workspace image separately from the server
  image. A `docker build -f Dockerfile.workspace` step is documented but not yet
  automated in CI.
- `shellCommand` in the config must be left unset (or empty) when using the
  workspace image, because setting it overrides the `CMD` and bypasses the
  default login shell invocation.
- Files created by the session user inside `/home/<username>` (the container
  layer, not the PVC) are lost when the pod is deleted. Only `/home/workspace`
  (the PVC mount) persists across sessions. Users should be made aware that
  dotfiles in `~` are ephemeral unless they symlink them into the PVC.
- Homebrew packages installed via `brew install` are also in the container layer
  and therefore ephemeral. This is acceptable for the homelab use-case; users who
  need persistent packages should bake them into a derived image.

## Alternatives considered

- **Fixed non-root user + dynamic username display** — originally rejected because
  the username would be hardcoded and would not match what the user typed. Adopted
  after reconsidering: fixing UID 1000 is acceptable because each workspace has a
  dedicated PVC (no collision risk), and the SSH username is surfaced in the prompt
  via `PS1` and the `USER`/`LOGNAME` env vars. This also satisfies `runAsNonRoot`.
- **Kubernetes `securityContext.runAsUser` (dynamic UID)** — rejected because we
  do not know the numeric UID of the SSH user at pod-creation time; we only know
  the string name. The fixed UID 1000 strategy (above) sidesteps this limitation.
- **`useradd` in `shellCommand`** — rejected because it would require the
  `shellCommand` to be a shell script fragment, breaking the clean separation
  between "what image to run" and "what command to run".
- **`initContainer` for user setup** — would allow a dynamic username with a
  non-root main container by writing a modified `/etc/passwd` to a shared
  emptyDir volume. Rejected as over-engineered: a fixed UID 1000 with a
  prompt override delivers the same user experience with a much simpler pod spec.
- **`entrypoint.sh` with `gosu`** — was the initial implementation. Rejected
  because it requires the container to start as root in order to call `useradd`
  and `gosu`, which conflicts with `runAsNonRoot: true`.
