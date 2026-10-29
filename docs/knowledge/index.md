---
okf_version: "0.2"
---

# voidshell project knowledge

voidshell provides GitHub-authenticated SSH access to Kubernetes workspaces for
homelabs, with disposable storage by default and opt-in persistent home volumes.

- [Project overview and configuration](../../README.md)
- [Identity and naming](../adr/001-identity-and-naming.md)
- [Workspace image and Linux identity](../adr/002-workspace-image-and-user-identity.md)
- [Storage selection](../adr/003-workspace-storage-selection.md)
- [Independent sessions, workspace lifecycle, and forwarding boundary](../adr/004-independent-sessions-and-workspace-lifecycle.md)
- [Operations and cluster smoke checks](../operations.md)
- [Security decisions](../security-decisions.md)
- [SSH compatibility and deferred work](../openssh-parity-plan.md)

These documents describe the intended implementation. Inspect the deployed
runtime to establish its current state.
