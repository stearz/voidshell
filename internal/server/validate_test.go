package server

import "testing"

func TestValidateSSHUsername(t *testing.T) {
	valid := []string{
		"alice",
		"bob123",
		"my-shell",
		"my_shell",
		"my.shell",
		"a",
		"A",
		"0a",
		"workspace-1",
		"x" + string(make([]byte, 62)), // 63 chars total (padded with zero bytes — replaced below)
	}
	// Build a proper 63-char string.
	long63 := "a"
	for range 62 {
		long63 += "b"
	}
	valid[len(valid)-1] = long63

	for _, u := range valid {
		if err := validateSSHUsername(u); err != nil {
			t.Errorf("validateSSHUsername(%q) unexpected error: %v", u, err)
		}
	}

	invalid := []string{
		"",                // empty
		"-shell",          // leading hyphen
		".shell",          // leading dot
		"shell!",          // exclamation
		"shell space",     // space
		"shell\nnewline",  // newline
		"shell;cmd",       // semicolon
		"shell$(inject)",  // shell metachar
		"shell`backtick`", // backtick
		long63 + "x",     // 64 chars — too long
	}

	for _, u := range invalid {
		if err := validateSSHUsername(u); err == nil {
			t.Errorf("validateSSHUsername(%q) expected error, got nil", u)
		}
	}
}
