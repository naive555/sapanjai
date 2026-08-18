package googlesheets

import (
	"context"
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
	sources map[uuid.UUID]oauth2.TokenSource
}

// NewTokenSourceCache builds an empty cache.
func NewTokenSourceCache() *TokenSourceCache {
	return &TokenSourceCache{sources: make(map[uuid.UUID]oauth2.TokenSource)}
}

// Get returns the cached TokenSource for connectorID, building and storing
// one from oauthCfg on first use. A later call with the same connectorID
// reuses the cached source regardless of the oauthCfg passed — callers must
// Delete a connector id whose credentials changed (e.g. after a
// PATCH /connectors/:id that rotates config) so the next Get rebuilds it.
func (c *TokenSourceCache) Get(ctx context.Context, connectorID uuid.UUID, oauthCfg OAuthConfig) oauth2.TokenSource {
	c.mu.Lock()
	defer c.mu.Unlock()

	if ts, ok := c.sources[connectorID]; ok {
		return ts
	}
	ts := NewTokenSource(ctx, oauthCfg)
	c.sources[connectorID] = ts
	return ts
}

// Delete evicts connectorID's cached TokenSource.
func (c *TokenSourceCache) Delete(connectorID uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sources, connectorID)
}
