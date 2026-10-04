# ADR 003: Workspace Storage Selection

**Status:** Accepted
**Date:** 2026-10-04

---

## Context

voidshell originally created a persistent volume claim (PVC) for every workspace.
That made persistence the implicit default, consuming durable storage even for
one-off shell sessions.

Users need an ordinary SSH connection to create a disposable workspace without
any per-user configuration, while still being able to opt into a durable
workspace when it is useful.

## Decision

The SSH username is a workspace selector. voidshell interprets it as follows:

```text
<workspace>          ephemeral workspace (default)
persist.<workspace>  persistent workspace
```

Examples:

```text
ssh project@voidshell.example.net
ssh persist.project@voidshell.example.net
```

The `persist.` prefix is removed before constructing the logical workspace
identity. Both examples above therefore use `project` as the workspace name,
but only the second creates and mounts a PVC.

An ephemeral workspace mounts an `emptyDir` at `/home/workspace` and creates no
PVC. Its contents disappear when voidshell deletes the workspace pod at session
end. A persistent workspace retains the existing stable PVC naming model, keyed
by the authenticated GitHub username and logical workspace name.

`persist.` without a following workspace name remains an ordinary ephemeral
workspace selector, so valid SSH username validation remains the single syntax
gate.

## Consequences

- Durable storage becomes explicit and opt-in.
- The server retains PVC create permissions because persistent workspaces remain
  supported.
- `storageClass` and `storageSize` configuration apply only to persistent
  workspaces.
- `persist.<workspace>` is a reserved selector syntax. Existing users of a
  literal workspace name beginning with that prefix must choose a different
  workspace name after upgrading.

## Alternatives considered

- **Hostnames:** SSH does not provide the server with a reliable requested host
  name, and aliases in SSH client configuration are invisible to voidshell.
  Separate hostnames would require separate listeners or proxy instances.
- **Persistent storage by default:** rejected because it allocates durable
  capacity for every incidental shell session.
- **Per-user server-side configuration:** rejected because the selection should
  be self-service and visible in the SSH command.
