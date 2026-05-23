# ADR 001: Workspace Identity Model and Kubernetes Object Naming

**Status:** Accepted  
**Date:** 2026-05-23  
**Source:** stearz/home-k8s#1046

---

## Context

voidshell proxies SSH connections to ephemeral Kubernetes workspaces. Each
workspace must map to exactly one user identity, and every Kubernetes object
created for that workspace must have a stable, unique name that is safe to use
as a label value, pod name, PVC name, and Service name.

Two inputs are available at connection time:

1. **Authenticated GitHub username** – the user whose public keys were accepted
   by the SSH server (resolved via the GitHub API).
2. **Requested SSH username** – the `user` field the SSH client sends (e.g.
   `alice`, `root`, or `myproject`). This lets one GitHub identity own multiple
   independent workspaces.

## Decision

### Identity model

The canonical workspace identity is the tuple:

```
(github_username, ssh_username)
```

Example: GitHub user `alice` connecting with SSH username `dev` resolves to the
workspace `alice/dev`.

The requested SSH username is **not** a second authentication factor. It is only
a workspace selector. Access control is based solely on the authenticated GitHub
username against the `allowedGitHubUsers` allow-list.

### Kubernetes object naming

Kubernetes names must be:

- **Lowercase** – `alice-dev`, not `Alice-Dev`.
- **DNS-safe** – only `[a-z0-9-]` characters; no leading/trailing hyphens.
- **Normalized** – any character outside `[a-z0-9]` is replaced with `-`;
  consecutive hyphens are collapsed to one.
- **Bounded** – at most 63 characters (RFC 1123 label limit).
- **Stable and unique** – include a short hash suffix derived from the canonical
  identity tuple so that renames or unusual characters in usernames cannot cause
  collisions.

The recommended name scheme for a workspace object is:

```
vs-<normalized-github>-<normalized-ssh>-<hash6>
```

Where `<hash6>` is the first 6 hex characters of `sha256(github_username + "/" + ssh_username)`.

Example: `alice` + `dev` →  `vs-alice-dev-3f9a2c`

The `vs-` prefix acts as a namespace guard against conflicts with other workloads
in the guest namespace.

## Consequences

- All code that creates Kubernetes objects must pass names through the normalizer
  before use.
- The normalizer and hash function must be deterministic and tested.
- The identity tuple must be logged at connection time for auditability.
- Guest namespace isolation still depends on Kubernetes RBAC; the naming scheme
  does not replace that.

## Alternatives considered

- **GitHub username only** – allows only one workspace per user; rejected.
- **Random UUIDs** – not stable across reconnects; rejected.
- **Full `username@github/ssh` string** – exceeds 63-char limit for long
  usernames; the hash suffix handles this case.
