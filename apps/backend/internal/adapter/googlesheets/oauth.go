package googlesheets

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// scopes requested for every google_sheets connector — read-only, matching
// the read-only MVP (docs/06-sheets-adapter.md). Write scopes are out of
// scope until Phase 2 (docs/07-sheets-adapter-plan.md §3).
var scopes = []string{
	"https://www.googleapis.com/auth/spreadsheets.readonly",
	"https://www.googleapis.com/auth/drive.readonly",
}

// NewTokenSource builds a TokenSource over one connector's stored refresh
// token. oauth2.ReuseTokenSource *is* the token cache: it holds the derived
// access token in memory and refreshes only on expiry. No Redis, no bespoke
// TTL logic — a refresh-derived access token is a live customer credential,
// and in-process memory is the right custody boundary for it.
func NewTokenSource(ctx context.Context, oauthCfg OAuthConfig) oauth2.TokenSource {
	conf := &oauth2.Config{
		ClientID:     oauthCfg.ClientID,
		ClientSecret: oauthCfg.ClientSecret,
		Endpoint:     google.Endpoint,
		Scopes:       scopes,
	}
	base := conf.TokenSource(ctx, &oauth2.Token{RefreshToken: oauthCfg.RefreshToken})
	return oauth2.ReuseTokenSource(nil, base)
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
// the one already cached.
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
func (c *TokenSourceCache) Get(ctx context.Context, connectorID uuid.UUID, oauthCfg OAuthConfig) oauth2.TokenSource {
	c.mu.Lock()
	defer c.mu.Unlock()

	fp := fingerprint(oauthCfg)
	if e, ok := c.sources[connectorID]; ok && e.fingerprint == fp {
		return e.source
	}
	ts := NewTokenSource(ctx, oauthCfg)
	c.sources[connectorID] = cacheEntry{source: ts, fingerprint: fp}
	return ts
}

// fingerprint reduces an OAuthConfig to a SHA-256 digest used only to
// detect that a connector's stored credential changed. It is never logged,
// returned, or persisted, and the digest is one-way — the credential itself
// stays in the TokenSource that needs it and nowhere else.
func fingerprint(oauthCfg OAuthConfig) [sha256.Size]byte {
	h := sha256.New()
	// Length-prefix each field so ("ab","c") and ("a","bc") cannot collide.
	for _, field := range []string{oauthCfg.ClientID, oauthCfg.ClientSecret, oauthCfg.RefreshToken} {
		_, _ = fmt.Fprintf(h, "%d:%s", len(field), field)
	}
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}

// Delete evicts connectorID's cached TokenSource.
func (c *TokenSourceCache) Delete(connectorID uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sources, connectorID)
}
