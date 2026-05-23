package workspace

import (
	"regexp"
	"strings"
	"testing"
)

var dnsLabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func TestWorkspaceID_ADRExample(t *testing.T) {
	id := New("alice", "dev")
	got := id.WorkspaceID()
	// Hash is deterministic: sha256("alice/dev")[:3] hex = 22805a.
	// (ADR 001 used an illustrative hash; this is the actual computed value.)
	want := "vs-alice-dev-22805a"
	if got != want {
		t.Errorf("WorkspaceID() = %q, want %q", got, want)
	}
}

func TestObjectNames(t *testing.T) {
	id := New("alice", "dev")
	wid := id.WorkspaceID()

	if got := id.PodName(); got != "shell-"+wid {
		t.Errorf("PodName() = %q, want %q", got, "shell-"+wid)
	}
	if got := id.PVCName(); got != "home-"+wid {
		t.Errorf("PVCName() = %q, want %q", got, "home-"+wid)
	}
}

func TestIsolation(t *testing.T) {
	cases := []struct {
		name    string
		a, b    Identity
	}{
		{
			"different github users, same ssh user",
			New("stearz", "someuser"),
			New("alice", "someuser"),
		},
		{
			"same github user, different ssh users",
			New("alice", "dev"),
			New("alice", "prod"),
		},
		{
			"different github users, different ssh users",
			New("alice", "dev"),
			New("bob", "dev"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.a.WorkspaceID() == tc.b.WorkspaceID() {
				t.Errorf("collision: both identities produced %q", tc.a.WorkspaceID())
			}
			if tc.a.PodName() == tc.b.PodName() {
				t.Errorf("pod name collision: %q", tc.a.PodName())
			}
			if tc.a.PVCName() == tc.b.PVCName() {
				t.Errorf("pvc name collision: %q", tc.a.PVCName())
			}
		})
	}
}

func TestNormalization(t *testing.T) {
	cases := []struct {
		name     string
		github   string
		ssh      string
		wantGH   string // expected normalized github segment inside workspace ID
		wantSSH  string // expected normalized ssh segment inside workspace ID
	}{
		{"lowercase", "ALICE", "DEV", "alice", "dev"},
		{"dots replaced", "alice.smith", "my.project", "alice-smith", "my-project"},
		{"underscores replaced", "alice_smith", "my_ws", "alice-smith", "my-ws"},
		{"leading/trailing hyphens stripped", "---alice---", "---dev---", "alice", "dev"},
		{"consecutive hyphens collapsed", "alice--smith", "dev--ws", "alice-smith", "dev-ws"},
		{"mixed special chars", "Alice@Example.com", "root@host", "alice-example-com", "root-host"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := New(tc.github, tc.ssh)
			wid := id.WorkspaceID()
			// workspace ID is: vs-<gh>-<ssh>-<hash6>
			// strip prefix "vs-" and suffix "-<hash6>"
			trimmed := strings.TrimPrefix(wid, "vs-")
			parts := strings.Split(trimmed, "-")
			// last element is the hash, rebuild segments
			hash := parts[len(parts)-1]
			if len(hash) != 6 {
				t.Fatalf("hash %q is not 6 chars", hash)
			}
			inner := strings.TrimSuffix(trimmed, "-"+hash)
			// find the boundary between gh and ssh segments using expected values
			boundary := tc.wantGH + "-" + tc.wantSSH
			if inner != boundary {
				t.Errorf("normalized segments = %q, want %q", inner, boundary)
			}
		})
	}
}

func TestLongUsernames(t *testing.T) {
	longGH := strings.Repeat("a", 100)
	longSSH := strings.Repeat("b", 100)
	id := New(longGH, longSSH)

	wid := id.WorkspaceID()
	if len(wid) > 63 {
		t.Errorf("WorkspaceID length %d > 63: %q", len(wid), wid)
	}
	if !dnsLabelRe.MatchString(wid) {
		t.Errorf("WorkspaceID %q is not a valid DNS label", wid)
	}

	// Different long usernames should still produce distinct IDs
	other := New(strings.Repeat("a", 100), strings.Repeat("a", 100))
	if id.WorkspaceID() == other.WorkspaceID() {
		t.Error("collision between different long usernames")
	}
}

func TestHostileUsernames(t *testing.T) {
	cases := []struct {
		name   string
		github string
		ssh    string
	}{
		{"all special chars", "!!!###", "???"},
		{"kubernetes injection attempt", "system:masters", "cluster-admin"},
		{"path traversal", "../../etc/passwd", "../../../../"},
		{"empty-like", "---", "---"},
		{"unicode", "üsêr", "wörk"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := New(tc.github, tc.ssh)
			wid := id.WorkspaceID()

			if len(wid) > 63 {
				t.Errorf("WorkspaceID length %d > 63: %q", len(wid), wid)
			}
			if !dnsLabelRe.MatchString(wid) {
				t.Errorf("WorkspaceID %q contains unsafe characters", wid)
			}
			if pod := id.PodName(); !regexp.MustCompile(`^[a-z0-9.-]+$`).MatchString(pod) {
				t.Errorf("PodName %q contains unsafe characters", pod)
			}
			if pvc := id.PVCName(); !regexp.MustCompile(`^[a-z0-9.-]+$`).MatchString(pvc) {
				t.Errorf("PVCName %q contains unsafe characters", pvc)
			}
		})
	}
}

func TestDNSLabelCompliance(t *testing.T) {
	ids := []Identity{
		New("alice", "dev"),
		New("stearz", "someuser"),
		New("Bob", "My_Project"),
		New(strings.Repeat("x", 50), strings.Repeat("y", 50)),
	}
	for _, id := range ids {
		for _, name := range []string{id.WorkspaceID()} {
			if !dnsLabelRe.MatchString(name) {
				t.Errorf("name %q is not a valid DNS label", name)
			}
			if len(name) > 63 {
				t.Errorf("name %q length %d > 63", name, len(name))
			}
		}
	}
}

func TestNormalizeSegment(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"alice", "alice"},
		{"ALICE", "alice"},
		{"alice-dev", "alice-dev"},
		{"alice_dev", "alice-dev"},
		{"alice.dev", "alice-dev"},
		{"alice--dev", "alice-dev"},
		{"---alice---", "alice"},
		{"!!!", "x"},
		{"", "x"},
		{strings.Repeat("a", 30), strings.Repeat("a", 26)},
		// truncation must not leave a trailing hyphen
		{"aaaaaaaaaaaaaaaaaaaaaaaaa-b", "aaaaaaaaaaaaaaaaaaaaaaaaa"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := normalizeSegment(tc.in, maxSegmentLen)
			if got != tc.want {
				t.Errorf("normalizeSegment(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
