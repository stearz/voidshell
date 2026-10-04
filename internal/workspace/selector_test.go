package workspace

import "testing"

func TestParseSelector(t *testing.T) {
	tests := []struct {
		username string
		wantMode StorageMode
		wantName string
	}{
		{username: "project", wantMode: StorageEphemeral, wantName: "project"},
		{username: "persist.project", wantMode: StoragePersistent, wantName: "project"},
		{username: "persist.my.project", wantMode: StoragePersistent, wantName: "my.project"},
		{username: "persist.", wantMode: StorageEphemeral, wantName: "persist."},
	}

	for _, tt := range tests {
		t.Run(tt.username, func(t *testing.T) {
			got := ParseSelector(tt.username)
			if got.Mode != tt.wantMode {
				t.Errorf("mode = %q, want %q", got.Mode, tt.wantMode)
			}
			if got.Name != tt.wantName {
				t.Errorf("name = %q, want %q", got.Name, tt.wantName)
			}
		})
	}
}
