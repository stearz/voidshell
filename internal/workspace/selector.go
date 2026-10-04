package workspace

import "strings"

// StorageMode controls whether a workspace receives durable storage.
type StorageMode string

const (
	// StorageEphemeral creates a workspace with session-only storage.
	StorageEphemeral StorageMode = "ephemeral"
	// StoragePersistent creates a workspace backed by a durable PVC.
	StoragePersistent StorageMode = "persistent"
)

// Selector is the storage mode and logical workspace name requested through the
// SSH username. "persist.<name>" opts into a persistent workspace; every other
// valid SSH username selects an ephemeral workspace.
type Selector struct {
	Mode StorageMode
	Name string
}

// ParseSelector converts an SSH username into a workspace selector.
func ParseSelector(username string) Selector {
	const persistentPrefix = "persist."
	if name, ok := strings.CutPrefix(username, persistentPrefix); ok && name != "" {
		return Selector{Mode: StoragePersistent, Name: name}
	}
	return Selector{Mode: StorageEphemeral, Name: username}
}
