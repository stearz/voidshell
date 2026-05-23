package auth

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// keyCache fetches and caches GitHub public keys per user.
//
// Cache entries expire after ttl. On expiry the keys are re-fetched on the
// next authentication attempt. If the re-fetch fails, authentication is
// rejected — stale entries are never used. This guarantees that removed
// users/keys stop working within one TTL even during partial GitHub outages.
// The trade-off is that a complete GitHub outage blocks all authentications
// until connectivity is restored.
type keyCache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
	ttl     time.Duration
	client  *http.Client
	baseURL string
}

type cacheEntry struct {
	keys      []ssh.PublicKey
	fetchedAt time.Time
}

func newKeyCache(ttl time.Duration, client *http.Client, baseURL string) *keyCache {
	return &keyCache{
		entries: make(map[string]cacheEntry),
		ttl:     ttl,
		client:  client,
		baseURL: baseURL,
	}
}

// keys returns the public keys for user, using the in-memory cache when the
// entry is still fresh.
func (c *keyCache) keys(ctx context.Context, user string) ([]ssh.PublicKey, error) {
	c.mu.Lock()
	entry, ok := c.entries[user]
	c.mu.Unlock()

	if ok && time.Since(entry.fetchedAt) < c.ttl {
		return entry.keys, nil
	}

	keys, err := c.fetchKeys(ctx, user)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.entries[user] = cacheEntry{keys: keys, fetchedAt: time.Now()}
	c.mu.Unlock()

	return keys, nil
}

func (c *keyCache) fetchKeys(ctx context.Context, user string) ([]ssh.PublicKey, error) {
	url := c.baseURL + "/" + user + ".keys"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building request for %q: %w", user, err)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching keys for %q: %w", user, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching keys for %q: HTTP %d", user, resp.StatusCode)
	}

	var keys []ssh.PublicKey
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		// ParseAuthorizedKey handles "ssh-ed25519 <base64> [comment]" lines.
		// Unrecognised or malformed lines are silently skipped.
		pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			continue
		}
		keys = append(keys, pk)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading keys for %q: %w", user, err)
	}
	return keys, nil
}
