package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"golang.org/x/crypto/ssh"
)

// ErrUnknownKey is returned when the offered key does not belong to any
// allowed GitHub user.
var ErrUnknownKey = errors.New("unknown key")

// ErrAmbiguousKey is returned when the offered key belongs to more than one
// allowed GitHub user.
var ErrAmbiguousKey = errors.New("ambiguous key: matches multiple allowed GitHub users")

// Authenticator verifies SSH public keys against GitHub user key lists.
type Authenticator struct {
	allowedUsers []string
	cache        *keyCache
	log          *slog.Logger
}

// New returns an Authenticator that allows SSH connections from the listed
// GitHub users. ttl controls how long fetched key lists are cached; once an
// entry expires it is re-fetched on the next auth attempt, so removed keys
// stop working within one TTL. Network failures during re-fetch cause
// authentication to be rejected (no grace period on stale data).
func New(allowedUsers []string, ttl time.Duration, log *slog.Logger) *Authenticator {
	return newWithBaseURL(allowedUsers, ttl, log, "https://github.com")
}

// newWithBaseURL is the internal constructor. It allows tests to point the
// fetcher at a local httptest.Server instead of real GitHub.
func newWithBaseURL(allowedUsers []string, ttl time.Duration, log *slog.Logger, baseURL string) *Authenticator {
	return &Authenticator{
		allowedUsers: allowedUsers,
		cache:        newKeyCache(ttl, &http.Client{Timeout: 15 * time.Second}, baseURL),
		log:          log,
	}
}

// Authenticate returns the authenticated GitHub username when offeredKey
// belongs to exactly one allowed GitHub user. sshUser is the SSH username the
// client requested; it is used only for logging and is not an auth factor.
// Private key material is never logged.
//
// Returns ErrUnknownKey if the key matches no allowed user.
// Returns ErrAmbiguousKey if the key matches more than one allowed user.
// Returns a wrapped error on network failure; treat this as a transient
// failure and reject the connection.
func (a *Authenticator) Authenticate(ctx context.Context, offeredKey ssh.PublicKey, sshUser string) (string, error) {
	offeredBytes := offeredKey.Marshal()

	var matchedUsers []string
	for _, ghUser := range a.allowedUsers {
		keys, err := a.cache.keys(ctx, ghUser)
		if err != nil {
			a.log.Warn("auth: key fetch failed, rejecting connection",
				"github_user", ghUser,
				"ssh_user", sshUser,
				"error", err,
			)
			return "", fmt.Errorf("fetching keys for %q: %w", ghUser, err)
		}
		for _, k := range keys {
			if bytes.Equal(k.Marshal(), offeredBytes) {
				matchedUsers = append(matchedUsers, ghUser)
				break
			}
		}
	}

	switch len(matchedUsers) {
	case 0:
		a.log.Info("auth: rejected — unknown key",
			"ssh_user", sshUser,
			"key_type", offeredKey.Type(),
		)
		return "", ErrUnknownKey
	case 1:
		a.log.Info("auth: accepted",
			"github_user", matchedUsers[0],
			"ssh_user", sshUser,
			"key_type", offeredKey.Type(),
		)
		return matchedUsers[0], nil
	default:
		a.log.Warn("auth: rejected — ambiguous key",
			"matched_users", matchedUsers,
			"ssh_user", sshUser,
			"key_type", offeredKey.Type(),
		)
		return "", ErrAmbiguousKey
	}
}
