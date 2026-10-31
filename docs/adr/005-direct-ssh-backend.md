# ADR 005 — Optional direct SSH data transport

## Decision

`workspace.backend` accepts `kubernetes` (the backward-compatible default) or
`ssh`. Both use the identity, storage selection, single coordinator, and lease
cleanup described in [ADR 004](004-independent-sessions-and-workspace-lifecycle.md).
Kubernetes remains responsible for workspace lifecycle. SSH mode sends shell,
command, SFTP/SCP, and loopback-forwarding bytes directly to the selected pod IP
on TCP 2222. It never falls back to exec when SSH fails.

A direct session channel acquires its lease when opened, including before its
first shell/command request. A forwarding channel still acquires a lease when
traffic opens it. Each channel has its own authenticated internal SSH transport;
closing it cannot disconnect another session. Host-key, handshake, and dialing
failures fail closed. Connect/handshake has a 15-second bound and context
cancellation closes the underlying socket. Process completion drains stdout and
stderr and releases the lease before forwarding exit status or exit signal.
Backend output EOF is forwarded once both output streams drain, even if the
process is still running; the input half remains open until the client ends it. Loopback forwarding also
propagates each direction's EOF separately and retains its lease until both
directions finish or the channel/transport is cancelled.

The proxy forwards validated shell/exec/SFTP, PTY, resize, and signal requests.
It rejects environment, agent, X11, unknown subsystem, and repeated start
requests. Both backends accept only loopback local forwarding destinations.
SSH mode always connects to IPv4 loopback *inside the selected workspace*;
remote forwarding remains unsupported. No client-selected cluster address is
dialed. SSH global requests and backend-created channels are not proxied.

## Credentials and rotation

Before readiness, the coordinator creates an immutable Secret with independent
Ed25519 host and client keys, a random name, and a controller owner reference to
the pod UID. Only the host key and authorized client **public** key are projected
into the pod; the client private key remains accessible only through the proxy's
Kubernetes permissions. The proxy checks the owner UID, authenticates as the
fixed `voidshell` Linux account, and pins the exact host public key from that
Secret. The verified endpoint and credentials are cached for the lease lifetime
and evicted before pod deletion, so new channels perform no API reads. A pod IP
reassigned unexpectedly cannot pass the pinned host-key check. This is neither trust on first use nor acceptance of arbitrary host keys.

Secrets are garbage-collected with their pods. New pod lifetimes generate fresh
keys, even for persistent workspaces; keys are never stored on home PVCs.
Routine rotation means close all channels and recreate the pod. Emergency
revocation means stop the proxy/active transports and delete the pod; replacing
keys does not revoke already established SSH transports. Crash recovery and
orphan-pod cleanup retain ADR 004's limitations. Namespace Secret-read and pod
administration privileges are trusted: compromise of either bypasses this
boundary. Kubernetes encryption at rest for Secrets is an operator concern.

## Image and readiness contract

The official image includes OpenSSH and `/usr/local/bin/voidshell-sshd`. The proxy
sets that command as PID 1 in SSH mode. The daemon runs as UID 1000 with privilege
escalation disabled and all capabilities dropped; it never starts as root.
Its host key is copied from the read-only Secret projection into a private
`/run/voidshell/sshd.*` directory because OpenSSH checks private-file ownership.
The image owns `/run/voidshell` as UID 1000 with mode 0700. TCP readiness checks
port 2222; every actual connection additionally checks authentication and identity.
OpenSSH permits 64 simultaneous unauthenticated handshakes before applying
its bounded startup-drop policy (`MaxStartups 64:30:128`), with a 15-second
authentication grace period. This accommodates concurrent channel startup while
bounding pending authentication work. The daemon stops with the pod. Background processes may remain while another
channel retains the workspace, as with Kubernetes exec.

Custom images must implement this startup script contract, include OpenSSH,
provide a non-locked `voidshell` account at UID 1000 with passwd home
`/home/workspace`, and allow that UID to write `/run/voidshell` and the mounted
home. Password authentication and PAM are disabled. The account password hash
`*` cannot authenticate a password but permits public-key login. No key may be
baked into an image; package-generated host keys are deleted in their build layer.
The official image's passwd shell is Bash, which handles SSH shell/exec requests;
`workspace.shellCommand` applies only to the Kubernetes backend. SFTP uses
OpenSSH's `internal-sftp`; SCP and rsync still need their binaries in the image.

## Network and deployment requirements

The proxy must route directly to pod IPs on TCP 2222. The Helm chart adds narrowly
selected proxy egress and workspace ingress rules across the configured namespaces.
This requires a CNI that enforces NetworkPolicy. Policies are additive: broader
existing policies can permit other sources, so operators must audit those too.
The workspace ingress rule isolates all ingress on matching workspace pods;
additional development-service exposure requires a deliberate separate policy.
Workspace egress remains an operator decision. The proxy still needs Kubernetes
API and GitHub access. Workspace ServiceAccount token automount is disabled for
both backends; it is unnecessary for shell sessions.

SSH mode grants `secrets/get,create` in the guest namespace and omits
`pods/exec,portforward` permissions. The external listener's host-key Secret stays
mounted as before. Backend changes interrupt active sessions; inspect and remove
old workspace pods first, keeping PVCs. An old pod with incompatible startup is
rejected rather than silently reused.

## Alternatives and validation

[ContainerSSH sshproxy](https://containerssh.io/v0.6/reference/sshproxy/) provides
separate internal authentication and host-key fingerprint verification. Its
backend configuration does not supply VoidShell's pod provisioning or lease
ownership. Reuse would introduce a second backend framework; this implementation
uses the existing `golang.org/x/crypto/ssh` dependency and a small request allowlist.
A sidecar was rejected because it would not share the workspace's container
filesystem and installed tooling. A shell/transfer hybrid keeps the exec data
path and adds another execution contract; both modes here handle all supported
channels consistently.

Docker integration tests exercise a real unprivileged OpenSSH daemon through the
VoidShell server, including hostile host keys, invalid credentials, PTY shell and
resize, signals, binary transfers, SCP, and forwarding restrictions. The fake
Kubernetes REST config is nil, so any accidental exec/port-forward fallback fails.
Cluster performance and API-server measurements are documented separately in
[backend benchmarks](../backend-benchmarks.md). The default remains Kubernetes;
changing it needs deployment evidence and a separate decision.
