package googlesheets

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// scopes requested for every google_sheets connector — read-only, matching
// the read-only MVP (docs/06-sheets-adapter.md). Write scopes are out of
// scope until Phase 2 (docs/07-sheets-adapter-decisions.md §3).
//
// Both credential variants use this same set: an OAuth refresh token was
// granted against it, and a service account signs its assertion for it
// (config.go's parseServiceAccount passes these to JWTConfigFromJSON).
// drive.readonly is the entry that makes it a Google *restricted* scope,
// which is what drives customers toward the service-account variant —
// see OAuthConfig's doc comment.
var scopes = []string{
	"https://www.googleapis.com/auth/spreadsheets.readonly",
	"https://www.googleapis.com/auth/drive.readonly",
}

// NewTokenSource builds a TokenSource over one connector's stored
// credential, whichever variant it holds.
//
// Both branches end in a reuse cache holding the derived access token in
// memory and refreshing only on expiry — explicitly via
// oauth2.ReuseTokenSource for the OAuth branch, and internally within
// jwt.Config.TokenSource for the service-account one. No Redis, no bespoke
// TTL logic: a derived access token is a live customer credential, and
// in-process memory is the right custody boundary for it.
//
// Infallible by construction. config.go parses the service-account key at
// ParseConfig time precisely so this function needs no error return, which
// keeps every tool call site free of an error path it would have nothing
// useful to do with.
func NewTokenSource(ctx context.Context, cred Credential) oauth2.TokenSource {
	switch {
	case cred.ServiceAccount != nil:
		// jwt.Config.TokenSource already wraps itself in a reuse cache, so
		// it is not wrapped again here.
		return cred.ServiceAccount.jwt.TokenSource(ctx)

	case cred.OAuth != nil:
		conf := &oauth2.Config{
			ClientID:     cred.OAuth.ClientID,
			ClientSecret: cred.OAuth.ClientSecret,
			Endpoint:     google.Endpoint,
			Scopes:       scopes,
		}
		base := conf.TokenSource(ctx, &oauth2.Token{RefreshToken: cred.OAuth.RefreshToken})
		return oauth2.ReuseTokenSource(nil, base)

	default:
		// Unreachable through ParseConfig, which rejects a credential-less
		// config outright. Reachable through a hand-built Credential, and
		// returning nil here would hand oauth2.NewClient a nil TokenSource
		// that panics on first use — inside a tools/call dispatch, where
		// CLAUDE.md requires a failing call to end cleanly rather than as a
		// panic or a raw Go error. An erroring source degrades it to an
		// ordinary tool error instead.
		return erroringTokenSource{}
	}
}

// erroringTokenSource is the zero-Credential fallback: every token request
// fails the same way, with a message naming no credential because there is
// none to name.
type erroringTokenSource struct{}

func (erroringTokenSource) Token() (*oauth2.Token, error) {
	return nil, errors.New("googlesheets: connector has no credential configured")
}

// TokenSourceCache holds one TokenSource per connector id, so a session
// calling several tools against the same connector reuses one access token
// instead of exchanging the refresh token each time. Safe for concurrent use.
//
// The health-check path does not use it: connector.Checker.Check receives a
// decrypted config but never a connector id, so it builds a TokenSource
// directly. Only the MCP tool handlers, which have an id in scope, come here.
type TokenSourceCache struct {
	mu      sync.Mutex
	sources map[uuid.UUID]cacheEntry
}

// cacheEntry pairs a connector's TokenSource with a fingerprint of the
// credential it was built from, so Get can tell a rotated credential from
// the one already cached — including a switch from one credential variant
// to the other.
type cacheEntry struct {
	source      oauth2.TokenSource
	fingerprint [sha256.Size]byte
}

// NewTokenSourceCache builds an empty cache.
func NewTokenSourceCache() *TokenSourceCache {
	return &TokenSourceCache{sources: make(map[uuid.UUID]cacheEntry)}
}

// Get returns the cached TokenSource for connectorID, building and storing
// one from oauthCfg on first use.
//
// Keyed by connector id *and* a fingerprint of oauthCfg, so a rotated
// credential takes effect on the very next call with nothing to invalidate
// by hand. That matters because the code that rotates a config (the REST
// handler) and the code that reads it (a tool handler, another module,
// another request) are different paths — a missed invalidation would keep
// minting tokens from a refresh token the customer believes is retired.
func (c *TokenSourceCache) Get(ctx context.Context, connectorID uuid.UUID, cred Credential) oauth2.TokenSource {
	c.mu.Lock()
	defer c.mu.Unlock()

	fp := fingerprint(cred)
	if e, ok := c.sources[connectorID]; ok && e.fingerprint == fp {
		return e.source
	}
	ts := NewTokenSource(ctx, cred)
	c.sources[connectorID] = cacheEntry{source: ts, fingerprint: fp}
	return ts
}

// fingerprint reduces a Credential to a SHA-256 digest used only to detect
// that a connector's stored credential changed. It is never logged,
// returned, or persisted, and the digest is one-way — the credential itself
// stays in the TokenSource that needs it and nowhere else.
//
// Every field that identifies the credential must be covered here. A
// fingerprint that ignored one would let TokenSourceCache keep serving a
// source built from a credential the customer has already retired: the
// rotation would appear to succeed and change nothing, which is the exact
// failure the cache's fingerprinting exists to prevent. The variant tag is
// part of that — switching a connector from OAuth to a service account
// must invalidate the entry even in the degenerate case where the rest of
// the material hashed identically.
func fingerprint(cred Credential) [sha256.Size]byte {
	h := sha256.New()
	switch {
	case cred.ServiceAccount != nil:
		// keyJSON, not the parsed jwt.Config: it is the whole of what the
		// customer supplied, so no field of the key can change without
		// changing it. This is the one thing config.go retains it for.
		writeFingerprintFields(h, "service_account", cred.ServiceAccount.keyJSON)
	case cred.OAuth != nil:
		writeFingerprintFields(h, "oauth", cred.OAuth.ClientID, cred.OAuth.ClientSecret, cred.OAuth.RefreshToken)
	default:
		writeFingerprintFields(h, "none")
	}
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}

// writeFingerprintFields hashes fields with a length prefix on each, so
// ("ab","c") and ("a","bc") cannot collide.
func writeFingerprintFields(h io.Writer, fields ...string) {
	for _, field := range fields {
		_, _ = fmt.Fprintf(h, "%d:%s", len(field), field)
	}
}

// Delete evicts connectorID's cached TokenSource.
func (c *TokenSourceCache) Delete(connectorID uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sources, connectorID)
}
