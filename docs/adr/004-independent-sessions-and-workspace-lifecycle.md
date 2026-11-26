# ADR 004 — Independent SSH processes and shared workspace lifecycle

## Decision

This document describes the default `kubernetes` backend.
[ADR 005](005-direct-ssh-backend.md) adds the optional direct SSH transport
while retaining the shared workspace lease and storage contracts.

A workspace pod hosts independent Kubernetes `pods/exec` processes for SSH shell,
command, and SFTP sessions. Its PID 1 is a small shell sleep loop, independent of
any client. `workspace.shellCommand` configures each shell session, not PID 1;
an empty command defaults to `/bin/bash -l`. Custom images need `/bin/sh`, `sleep`,
`env`, and the configured shell. Custom image entrypoints are not run.

Each active session or `direct-tcpip` channel holds a workspace lease, keyed by
pod name (which includes storage mode). The first lease provisions the pod and
waits for the workspace container to become ready. Creation and deletion are
serialized per workspace, including across separate SSH connections. Unrelated
workspaces provision independently.

The last lease release starts a process-local idle timer. `workspace.idleTimeout`
defaults to `0s` (immediate cleanup for backward compatibility), accepts
non-negative Go durations, and can be overridden with
`VOIDSHELL_WORKSPACE_IDLE_TIMEOUT`. Set `30m` for T3 Code. A new active
channel cancels the timer; the next last release starts a full new idle window.
The pod and background processes survive gaps between commands and forwarded
requests, including T3 Code's separate backend launch and tunnel connections.
Active workspaces have no maximum lifetime. Expiry and acquisition use the same
per-workspace gate: a stale timer cannot delete a resumed workspace, and a new
acquisition waits for any deletion already in progress.

An open SSH transport without active channels does not reset the idle timer.
Forward-only connections provision lazily when traffic first opens a forwarding
channel; an idle `ssh -N -L ...` does not provision a pod. Keep an active channel
open if a development server must remain available beyond the idle window.
Graceful proxy shutdown bypasses idle timers, cleans retained pods in parallel,
and waits for cleanup callbacks. Failed setup still cleans partial pods
immediately. Timers are not persisted: abrupt crashes can leave orphaned pods.

A completed command sends its exit status after releasing its lease. Remote
process exit codes are preserved, while infrastructure failures return status 1
with a diagnostic on stderr. Failed setup also attempts pod cleanup. PVCs are
never deleted. Channel and transport closure cancel the corresponding API
streams; cancellation does not guarantee termination of background processes
inside a pod retained by another session or the idle grace period. Final pod
deletion terminates them.

The home volume is mounted at `/home/workspace`, which is also `HOME` and the
process working directory. Pod `fsGroup: 1000` makes supported volumes writable
by the non-root workspace user. Custom CSI drivers must support this ownership
model. Homebrew remains part of the container filesystem and is not persisted.

## Deployment constraint

Lease coordination is in memory. Exactly one voidshell instance may manage a
given guest namespace. The Helm chart rejects `replicaCount` other than 1 and
uses the `Recreate` deployment strategy to avoid overlapping coordinators during
upgrades. The proxy waits for connection cleanup on shutdown; the chart allows
45 seconds, including a 30-second pod cleanup timeout.

This does not provide crash recovery or distributed ownership. An abrupt proxy
crash can leave orphaned pods. Operators must inspect and remove idle workspace
pods when migrating from the previous attach-based execution model; active SSH
sessions are interrupted by an upgrade. PVC data remains intact.

## SSH features and trust boundary

- `exec` passes the original command to `/bin/sh -c`, preserving quoting and
  pipelines. Commands execute inside the authenticated user's workspace.
- SFTP runs the image's `sftp-server` without a PTY. The proxy probes known
  OpenSSH paths using a fixed command and keeps protocol stdout free of banners.
- Local forwarding accepts only `localhost`, `127.0.0.1`, and `::1` destinations
  with ports 1–65535. These names select the workspace's loopback, not an address
  resolved or dialed by the proxy. Kubernetes `pods/portforward` provides the
  transport, including for services bound only to loopback. The IP family used
  by the kubelet is implementation-dependent.
- Every forwarding channel uses the authenticated identity and selected storage
  mode. Arbitrary cluster destinations and remote forwarding remain unsupported.
- `TERM` is passed per PTY process. Other client environment requests are rejected.
  Signal forwarding, agent forwarding, and X11 forwarding remain unsupported.
- The proxy needs `pods/exec` and `pods/portforward` create permissions; it no
  longer needs `pods/attach`.

## Consequences

Multiple terminals and file transfers can share one workspace without sharing a
shell process or deleting each other's pod. Ephemeral data remains available
while channels are active and during the idle grace period, and is discarded
when the pod is deleted.
Persistent workspaces keep only the PVC-backed home across pod lifetimes.

Multi-replica deployment or recovery of idle deadlines across proxy restarts
requires a separate coordination and lifecycle design.
