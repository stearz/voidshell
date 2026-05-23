package workspace

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
)

var unsafeChars = regexp.MustCompile(`[^a-z0-9]+`)

// maxSegmentLen is the per-segment character budget.
// Formula: 63 (max label) - 3 (vs-) - 1 (sep) - 1 (sep) - 6 (hash) = 52, split evenly.
const maxSegmentLen = 26

// Identity is the canonical (githubUser, sshUser) workspace tuple.
type Identity struct {
	GithubUser string
	SSHUser    string
}

// New creates an Identity from the authenticated GitHub username and the requested SSH username.
func New(githubUser, sshUser string) Identity {
	return Identity{GithubUser: githubUser, SSHUser: sshUser}
}

// WorkspaceID returns a Kubernetes-safe workspace identifier of the form
// vs-<normalized-github>-<normalized-ssh>-<hash6>, bounded to 63 characters.
func (id Identity) WorkspaceID() string {
	gh := normalizeSegment(id.GithubUser, maxSegmentLen)
	ssh := normalizeSegment(id.SSHUser, maxSegmentLen)
	hash := computeHash(id.GithubUser, id.SSHUser)
	return fmt.Sprintf("vs-%s-%s-%s", gh, ssh, hash)
}

// PodName returns the Kubernetes pod name for this workspace.
func (id Identity) PodName() string {
	return "shell-" + id.WorkspaceID()
}

// PVCName returns the Kubernetes PVC name for this workspace.
func (id Identity) PVCName() string {
	return "home-" + id.WorkspaceID()
}

// normalizeSegment converts a raw username to a lowercase, DNS-safe string
// truncated to maxLen characters. An all-invalid input produces "x".
func normalizeSegment(s string, maxLen int) string {
	s = strings.ToLower(s)
	s = unsafeChars.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > maxLen {
		s = strings.TrimRight(s[:maxLen], "-")
	}
	if s == "" {
		s = "x"
	}
	return s
}

// computeHash returns the first 6 hex characters of sha256(github + "/" + ssh).
func computeHash(github, ssh string) string {
	h := sha256.Sum256([]byte(github + "/" + ssh))
	return fmt.Sprintf("%x", h[:3])
}
