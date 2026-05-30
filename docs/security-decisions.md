# Security Decisions

This document records security issues identified during review and the decisions made for each.
Issues are presented in order: architectural first, then implementation-specific.

---

## SEC-001 — NetworkPolicy disabled by default

**Category:** Architectural  
**Status:** Fixed

**Issue:**  
The Helm chart shipped a `NetworkPolicy` resource but it was disabled by default
(`networkPolicy.enabled: false`). Any pod in the cluster could reach the SSH service
without restriction.

Additionally, the policy had no egress rules, meaning the voidshell pod could freely
reach private network ranges — making it a potential pivot point for lateral movement
to internal services.

**Decision:**  
Enable the NetworkPolicy by default and add egress rules that block RFC 1918 private
ranges (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `100.64.0.0/10`).
DNS (UDP/TCP port 53) is always permitted so GitHub API hostname resolution keeps
working. Users can open specific private CIDRs back up via
`networkPolicy.egressPrivateExceptions` in `values.yaml` — required at minimum for
the Kubernetes API server address.

**Changes:**  
- `charts/voidshell/templates/networkpolicy.yaml` — added `Egress` policy type and
  egress rules; enabled by default.
- `charts/voidshell/values.yaml` — `networkPolicy.enabled` flipped to `true`;
  added `networkPolicy.egressPrivateExceptions` list with documented examples.

---

## SEC-002 — GitHub API as sole authentication dependency

**Category:** Architectural  
**Status:** Accepted risk (no fix planned)

**Issue:**  
Every new SSH connection triggers a call to `https://github.com/<username>.keys` to
validate the offered public key. If GitHub is unreachable (outage, DNS failure,
network partition), all authentications fail — including for users who have connected
successfully many times before. There is no offline fallback and no way to pre-seed
the cache.

**Mitigating factors:**
- The key cache (default TTL 5 minutes) absorbs brief interruptions; existing cached
  entries remain valid until they expire.
- This is a deliberate design choice: GitHub is the identity provider, and the
  system inherits its availability characteristics.

**Decision:**  
Accepted as a known limitation. For the target use-case (homelab / small team) this
tradeoff is acceptable. A future version could explore a persistent key cache or an
alternative identity provider, but no fix is planned at this time.

---

## SEC-003 — Passwordless sudo in workspace containers

**Category:** Architectural  
**Status:** Fixed

**Issue:**  
`Dockerfile.workspace` installed `sudo` and granted the `voidshell` user
`NOPASSWD:ALL`. The workspace pod security context only set `RunAsUser: 1000` and
`RunAsNonRoot: true` but did not set `allowPrivilegeEscalation: false`, meaning the
`no_new_privs` kernel bit was not set. As a result `sudo` was fully functional and
any user could escalate to root inside their container.

**Decision:**  
Homebrew is already available in the workspace image for all package installs, so
root access is not needed. Removed `sudo` entirely (both the package and the
sudoers entry). Hardened the workspace pod security context to match the proxy pod:
`allowPrivilegeEscalation: false` and `capabilities: drop ALL`.

**Changes:**  
- `Dockerfile.workspace` — removed `sudo` from apt-get install; removed
  `useradd` sudoers setup.
- `internal/k8s/lifecycle.go` — added `AllowPrivilegeEscalation: false` and
  `Capabilities{Drop: ["ALL"]}` to the workspace container `SecurityContext`.

---

## SEC-004 — No SSH host key rotation mechanism

**Category:** Architectural  
**Status:** Accepted risk (no fix planned)

**Issue:**  
The SSH host key is loaded once at startup from a Kubernetes Secret. Rotating it
requires updating the Secret and restarting the pod. Every existing client will see
a host key mismatch on their next connection and must manually clear `known_hosts`.

**Decision:**  
Accepted for the target use-case (homelab / small team). Manual rotation via
`kubectl` is sufficient. No automated rotation mechanism is planned.

---

## SEC-005 — No structured audit logging

**Category:** Architectural  
**Status:** Deferred

**Issue:**  
There is no structured audit trail for workspace lifecycle events (workspace
creation, pod assignment, deletion). Investigating "who ran something at time X"
relies on Kubernetes pod events, which have limited retention and no central
aggregation.

**Decision:**  
Deferred to a future version. Structured audit logging (workspace create/delete
with GitHub username, timestamp, pod name) should be added when the project
matures beyond homelab use.

---

## SEC-006 — RELEASE_TOKEN scope in release-please workflow

**Category:** Implementation  
**Status:** Accepted (scopes are minimal)

**Issue:**  
`.github/workflows/release-please.yml` uses a `secrets.RELEASE_TOKEN` PAT. An
overly broad token could give an attacker wide repository or org access if leaked.

**Decision:**  
Token is scoped to read/write on code and pull-requests only — the minimum required
for release-please to create release PRs and tags. No action needed.

---

## SEC-007 — No container image scanning in CI

**Category:** Implementation  
**Status:** Fixed

**Issue:**  
The CI pipeline built and pushed container images without scanning them for known
CVEs. A vulnerable OS package or dependency could be silently published to GHCR.

**Decision:**  
Added a `scan` job to `.github/workflows/ci.yml` using Trivy (open-source, no cost).
It builds both the `voidshell` and `voidshell-workspace` images (amd64) and scans
them for CRITICAL and HIGH severity findings where a fix is available
(`ignore-unfixed: true`). Results are uploaded as SARIF to the GitHub Security tab.
The `build` job now depends on `scan`, so a failing scan blocks the image push.

**Changes:**  
- `.github/workflows/ci.yml` — added `scan` job; added `scan` to `build.needs`.

---

## SEC-008 — Workspace PVC lifecycle / no TTL

**Category:** Implementation  
**Status:** Deferred

**Issue:**  
Workspace PVCs are created on first login and never automatically deleted. Removing
a user from `allowedGitHubUsers` blocks new sessions but leaves their PVC (and all
data) in the guest namespace indefinitely, accumulating unclaimed storage with no
inventory or expiry.

**Decision:**  
Deferred. To be addressed as part of a future session management feature that gives
users self-service control over their workspace: reset PVC, back up to remote
storage, or download as a zip archive. Operator-side cleanup (orphan detection,
TTL-based deletion) should be added in the same scope.

---

## SEC-009 — Opinionated workspace image as default

**Category:** Implementation  
**Status:** Fixed

**Issue:**  
The Helm chart defaulted `workspace.shellImage` to `ubuntu:26.04` (plain Ubuntu)
while the actual published workspace image (`Dockerfile.workspace`) bakes in
Homebrew, build tools, and a package manager — a considerably larger attack surface.
The two were inconsistent, and the larger image was not the default despite being the
intended experience.

**Decision:**  
Build and push the workspace image (`ghcr.io/stearz/voidshell-workspace`) from CI
alongside the main image, make it the default, and give operators a clear opt-out by
setting `workspace.image` to any lighter image (e.g. `ubuntu:26.04`). Renamed the
value from `workspace.shellImage` to `workspace.image` for consistency. The env var
override is now `VOIDSHELL_WORKSPACE_IMAGE`.

**Changes:**  
- `charts/voidshell/values.yaml` — `workspace.shellImage` → `workspace.image`,
  default set to `ghcr.io/stearz/voidshell-workspace:main`.
- `charts/voidshell/templates/configmap.yaml` — updated template key.
- `configs/voidshell.example.yaml` — updated key and default value.
- `internal/config/config.go` — renamed `WorkspaceConfig.ShellImage` → `Image`,
  YAML tag `shellImage` → `image`, env var `VOIDSHELL_WORKSPACE_SHELL_IMAGE` →
  `VOIDSHELL_WORKSPACE_IMAGE`, default updated.
- `internal/k8s/lifecycle.go` — renamed `Config.ShellImage` → `Image`.
- `cmd/voidshell/main.go`, `internal/config/config_test.go`,
  `internal/k8s/lifecycle_test.go` — updated all references.
- `.github/workflows/ci.yml` — added workspace image build and push step to the
  `build` job (`ghcr.io/stearz/voidshell-workspace`).

---

## SEC-010 — SSH username not validated

**Category:** Implementation  
**Status:** Fixed

**Issue:**  
The SSH username was accepted verbatim and passed as `USER`/`LOGNAME` env vars into
workspace pods without any sanitisation. Characters such as spaces, semicolons,
newlines, or shell metacharacters were accepted by the SSH handshake and could land
in the pod environment.

**Decision:**  
Validate the SSH username at the earliest possible point — in the `PublicKeyCallback`
before the GitHub API call — using an allowlist regex:
`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`. Invalid usernames are rejected during the SSH
handshake and no auth or pod work is performed.

**Changes:**  
- `internal/server/validate.go` — new file with `validateSSHUsername`.
- `internal/server/server.go` — call `validateSSHUsername` in `PublicKeyCallback`.
- `internal/server/validate_test.go` — unit tests for valid/invalid cases.
- `internal/server/server_test.go` — integration test verifying bad usernames are
  rejected at the handshake and never reach `EnsureWorkspace`.

---

<!-- Next issues to be added here as the review progresses -->
