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
