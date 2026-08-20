package googlesheets

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func testOAuthConfig() OAuthConfig {
	return OAuthConfig{
		RefreshToken: "1//0g-refresh",
		ClientID:     "abc.apps.googleusercontent.com",
		ClientSecret: "shh",
	}
}

// TestNewTokenSource_NoNetworkCall proves construction alone never touches
// the network — oauth2.Config.TokenSource / oauth2.ReuseTokenSource are lazy
// and only call out to Google when .Token() is invoked, which this test
// never does. This unit test suite must never touch the network.
func TestNewTokenSource_NoNetworkCall(t *testing.T) {
	ts := NewTokenSource(context.Background(), testOAuthConfig())
	if ts == nil {
		t.Fatal("NewTokenSource returned nil")
	}
}

func TestTokenSourceCache_ReusesSourcePerConnector(t *testing.T) {
	cache := NewTokenSourceCache()
	id := uuid.New()

	first := cache.Get(context.Background(), id, testOAuthConfig())
	second := cache.Get(context.Background(), id, testOAuthConfig())

	if first != second {
		t.Fatal("Get for the same connector id returned two different TokenSources; expected the cached instance to be reused")
	}
}

func TestTokenSourceCache_DifferentConnectorsGetDifferentSources(t *testing.T) {
	cache := NewTokenSourceCache()

	a := cache.Get(context.Background(), uuid.New(), testOAuthConfig())
	b := cache.Get(context.Background(), uuid.New(), testOAuthConfig())

	if a == b {
		t.Fatal("Get for two different connector ids returned the same TokenSource")
	}
}

// TestTokenSourceCache_RotatedCredentialRebuilds is the regression test for
// a bug found reviewing step 6: the cache keyed only on connector id, so a
// customer who rotated a leaked refresh token through PATCH /connectors/:id
// kept being served the TokenSource built from the OLD credential until the
// process restarted. Nothing in production ever called Delete, and the two
// code paths involved (the REST handler that rotates, the MCP tool handler
// that reads) are in different modules on different requests, so no manual
// eviction was ever going to happen reliably.
func TestTokenSourceCache_RotatedCredentialRebuilds(t *testing.T) {
	cache := NewTokenSourceCache()
	id := uuid.New()

	leaked := testOAuthConfig()
	rotated := testOAuthConfig()
	rotated.RefreshToken = "1//0g-rotated-after-a-leak"

	before := cache.Get(context.Background(), id, leaked)
	after := cache.Get(context.Background(), id, rotated)

	if before == after {
		t.Fatal("a rotated refresh token still resolved to the TokenSource built from the retired credential")
	}

	// ...while an unchanged credential must still hit the cache, which is
	// the whole point of holding one.
	again := cache.Get(context.Background(), id, rotated)
	if again != after {
		t.Fatal("an unchanged credential rebuilt its TokenSource instead of reusing the cached one")
	}
}

// A rotation of the client secret alone (same refresh token) must also
// rebuild — all three credential fields feed the fingerprint.
func TestTokenSourceCache_RotatedClientSecretRebuilds(t *testing.T) {
	cache := NewTokenSourceCache()
	id := uuid.New()

	original := testOAuthConfig()
	rotated := testOAuthConfig()
	rotated.ClientSecret = "rotated-secret"

	if cache.Get(context.Background(), id, original) == cache.Get(context.Background(), id, rotated) {
		t.Fatal("a rotated client secret still resolved to the previously cached TokenSource")
	}
}

func TestTokenSourceCache_DeleteEvicts(t *testing.T) {
	cache := NewTokenSourceCache()
	id := uuid.New()

	first := cache.Get(context.Background(), id, testOAuthConfig())
	cache.Delete(id)
	second := cache.Get(context.Background(), id, testOAuthConfig())

	if first == second {
		t.Fatal("Get after Delete returned the same TokenSource; expected a fresh one to be built")
	}
}
