package server

import (
	"fmt"
	"regexp"
)

// validSSHUsername matches usernames that are safe to use as workspace selectors
// and env-var values: alphanumeric start, then alphanumeric / dot / hyphen /
// underscore, max 63 chars (DNS label limit).
var validSSHUsername = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

func validateSSHUsername(user string) error {
	if !validSSHUsername.MatchString(user) {
		return fmt.Errorf("invalid SSH username %q: must start with a letter or digit and contain only [a-zA-Z0-9._-], max 63 chars", user)
	}
	return nil
}
