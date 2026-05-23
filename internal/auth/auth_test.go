package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// generateKey creates a fresh Ed25519 key pair and returns the SSH public key.
func generateKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	rawPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating ed25519 key: %v", err)
	}
	pub, err := ssh.NewPublicKey(rawPub)
	if err != nil {
		t.Fatalf("wrapping public key: %v", err)
	}
	return pub
}

// authorizedLine serializes pub to a single authorized_keys line.
func authorizedLine(pub ssh.PublicKey) string {
	return string(ssh.MarshalAuthorizedKey(pub))
}

// newTestAuth creates an Authenticator that fetches keys from srv instead of GitHub.
func newTestAuth(allowedUsers []string, ttl time.Duration, srv *httptest.Server) *Authenticator {
	return newWithBaseURL(allowedUsers, ttl, slog.Default(), srv.URL)
}

func TestAuthSuccess(t *testing.T) {
	aliceKey := generateKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/alice.keys" {
			w.Write([]byte(authorizedLine(aliceKey))) //nolint:errcheck
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a := newTestAuth([]string{"alice"}, time.Minute, srv)
	ghUser, err := a.Authenticate(context.Background(), aliceKey, "myworkspace")
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if ghUser != "alice" {
		t.Errorf("expected github_user=alice, got %q", ghUser)
	}
}

func TestAuthUnknownKey(t *testing.T) {
	aliceKey := generateKey(t)
	unknownKey := generateKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/alice.keys" {
			w.Write([]byte(authorizedLine(aliceKey))) //nolint:errcheck
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a := newTestAuth([]string{"alice"}, time.Minute, srv)
	_, err := a.Authenticate(context.Background(), unknownKey, "myworkspace")
	if err != ErrUnknownKey {
		t.Errorf("expected ErrUnknownKey, got %v", err)
	}
}

// TestAuthDisallowedUser verifies that a key belonging only to a GitHub user
// not in allowedGitHubUsers is rejected.
func TestAuthDisallowedUser(t *testing.T) {
	aliceKey := generateKey(t)
	eveKey := generateKey(t) // eve is not in the allowed list

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/alice.keys" {
			w.Write([]byte(authorizedLine(aliceKey))) //nolint:errcheck
		} else {
			// eve.keys would be here but we never fetch it
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// Only alice is allowed; eve's key should never match.
	a := newTestAuth([]string{"alice"}, time.Minute, srv)
	_, err := a.Authenticate(context.Background(), eveKey, "myworkspace")
	if err != ErrUnknownKey {
		t.Errorf("expected ErrUnknownKey for disallowed user, got %v", err)
	}
}

// TestAuthCacheExpiry verifies that a key removed from GitHub stops working
// after the cache TTL expires.
func TestAuthCacheExpiry(t *testing.T) {
	aliceKey := generateKey(t)
	keysBody := authorizedLine(aliceKey) // start: alice has this key

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/alice.keys" {
			w.Write([]byte(keysBody)) //nolint:errcheck
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	const ttl = 80 * time.Millisecond
	a := newTestAuth([]string{"alice"}, ttl, srv)

	// First authentication succeeds.
	if _, err := a.Authenticate(context.Background(), aliceKey, "ws"); err != nil {
		t.Fatalf("first auth: %v", err)
	}

	// Simulate the key being removed on the GitHub side.
	keysBody = ""

	// Within the TTL the cached entry is still fresh — should still succeed.
	if _, err := a.Authenticate(context.Background(), aliceKey, "ws"); err != nil {
		t.Fatalf("auth within TTL after key removal: %v", err)
	}

	// Wait for the cache entry to expire.
	time.Sleep(3 * ttl)

	// After expiry the refreshed (empty) key list is used — auth must fail.
	_, err := a.Authenticate(context.Background(), aliceKey, "ws")
	if err != ErrUnknownKey {
		t.Errorf("expected ErrUnknownKey after TTL expiry, got %v", err)
	}
}

// TestAuthAmbiguousKey verifies that a key shared by two allowed users is rejected.
func TestAuthAmbiguousKey(t *testing.T) {
	sharedKey := generateKey(t) // same key registered under both alice and bob

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/alice.keys", "/bob.keys":
			w.Write([]byte(authorizedLine(sharedKey))) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a := newTestAuth([]string{"alice", "bob"}, time.Minute, srv)
	_, err := a.Authenticate(context.Background(), sharedKey, "myworkspace")
	if err != ErrAmbiguousKey {
		t.Errorf("expected ErrAmbiguousKey, got %v", err)
	}
}
