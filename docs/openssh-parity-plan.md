# SSH compatibility status and remaining work

The execution and lifecycle contract is recorded in
[ADR 004](adr/004-independent-sessions-and-workspace-lifecycle.md).

## Implemented

| Feature | Implementation |
|---|---|
| Interactive shell | Independent `pods/exec` process per session |
| PTY, TERM, window resize | Per-process terminal and resize queue |
| Remote command | `/bin/sh -c`, distinct stderr without PTY, remote exit status |
| SFTP and modern SCP | Fixed subsystem wrapper discovers OpenSSH `sftp-server` |
| Legacy SCP and rsync | Remote exec; binaries supplied in the workspace image |
| Local port forwarding | Kubernetes pod port-forward streams, workspace loopback only |
| Keepalive requests | Replies to `keepalive@openssh.com` |
| Concurrent sessions | Process-local workspace leases; cleanup after last active channel |
| Disconnect and setup failure | Cancel API streams and attempt cleanup; retain PVCs |

## Deliberately deferred

- Remote port forwarding (`ssh -R`): requires a separate decision about listener
  placement and exposure. Binding a listener on the proxy does not create a
  listener inside a workspace.
- Arbitrary destination forwarding: would broaden the trust boundary beyond the
  user's own workspace.
- Client environment variables beyond PTY `TERM`: requires an explicit allowlist.
- SSH signal forwarding: Kubernetes exec does not expose the remote process ID.
- Idle TTL, automatic crash recovery, and multi-replica coordination.
- VS Code Remote SSH compatibility: needs separate end-to-end validation; the
  implemented primitives alone do not establish support.

## Validation

Automated tests cover real SSH channels with a controlled workspace backend,
including exit codes and binary streams, overlapping connections, disconnects,
and rejected forwarding destinations. Kubernetes transport tests inspect exec
argv and stream flags and exercise SPDY port-forward data/error streams and
half-close against a local HTTP server. These do not replace the cluster smoke
checks in [operations](operations.md).
