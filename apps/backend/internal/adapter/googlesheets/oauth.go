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

// NewTokenSource builds an oauth2.TokenSource over one connector's stored
// refresh token, wrapped in oauth2.ReuseTokenSource. That wrapper *is* the
// token cache (docs/07-sheets-adapter-plan.md step 5) — it holds the
// derived access token in memory and only calls out to Google to refresh it
// once it is expired, so a caller issuing several requests in a row reuses
// one access token instead of exchanging the refresh token on every call.
// No Redis, no bespoke TTL logic: a refresh-derived access token is a live
// customer credential, and in-process memory (never persisted, never
// serialized) is the right custody boundary for it.
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

// TokenSourceCache holds one TokenSource per connector id so a session that
// calls several tools against the same connector (steps 6+) reuses the
// cached access token across calls instead of exchanging the refresh token
// every time. Safe for concurrent use.
//
// Note on this step's own use of the cache: internal/module/connector.
// Checker.Check receives only a connector's decrypted config, never its id
// (see checker.go's doc comment), so the health-check path builds a
// TokenSource directly via NewTokenSource rather than through this cache.
// The cache exists here, in step 5, as the seam step 6+'s MCP tool
// handlers — which do have a connector id in scope for the whole session —
// are expected to call.
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
// The entry is keyed by connector id *and* a fingerprint of oauthCfg, so a
// credential rotated through PATCH /connectors/:id takes effect on the very
// next call: the fingerprint no longer matches, the stale source is
// discarded, and a fresh one is built from the new credential. Relying on
// callers to Delete an entry by hand would be a footgun — the caller that
// rotates a connector's config (the REST handler) and the caller that reads
// it (an MCP tool handler in another module, on another request) are not
// the same code path, so a missed Delete would silently keep minting access
// tokens from a refresh token the customer believes they have retired.
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
