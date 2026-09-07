package googlesheets

import (
	"context"
	"crypto/sha256"
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

// oauthCred wraps an OAuthConfig as a Credential. It takes its argument by
// value and addresses the copy, so two calls never alias — a shared pointer
// would let a test mutating one credential silently change another.
func oauthCred(cfg OAuthConfig) Credential {
	return Credential{OAuth: &cfg}
}

// serviceAccountCred builds a service-account Credential through ParseConfig
// rather than by hand: jwt and keyJSON are unexported, and going through the
// real parser is also what proves the two layers agree.
func serviceAccountCred(t *testing.T, keyJSON string) Credential {
	t.Helper()
	cfg, err := ParseConfig(map[string]any{
		"service_account": map[string]any{"key_json": keyJSON},
		"scope":           map[string]any{"spreadsheet_ids": []any{"sheet-a"}},
	})
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	return cfg.Credential
}

// keyJSONWithPrivateKey builds a well-formed service-account key file
// carrying a caller-chosen private key, so a test can express "the same
// account, a rotated key".
func keyJSONWithPrivateKey(privateKey string) string {
	return `{"type":"service_account","project_id":"example-project",` +
		`"client_email":"` + testServiceAccountEmail + `",` +
		`"private_key":"` + privateKey + `",` +
		`"token_uri":"https://oauth2.googleapis.com/token"}`
}

// TestNewTokenSource_NoNetworkCall proves construction alone never touches
// the network — oauth2.Config.TokenSource / oauth2.ReuseTokenSource are lazy
// and only call out to Google when .Token() is invoked, which this test
// never does. This unit test suite must never touch the network.
func TestNewTokenSource_NoNetworkCall(t *testing.T) {
	ts := NewTokenSource(context.Background(), oauthCred(testOAuthConfig()))
	if ts == nil {
		t.Fatal("NewTokenSource returned nil")
	}
}

func TestTokenSourceCache_ReusesSourcePerConnector(t *testing.T) {
	cache := NewTokenSourceCache()
	id := uuid.New()

	first := cache.Get(context.Background(), id, oauthCred(testOAuthConfig()))
	second := cache.Get(context.Background(), id, oauthCred(testOAuthConfig()))

	if first != second {
		t.Fatal("Get for the same connector id returned two different TokenSources; expected the cached instance to be reused")
	}
}

func TestTokenSourceCache_DifferentConnectorsGetDifferentSources(t *testing.T) {
	cache := NewTokenSourceCache()

	a := cache.Get(context.Background(), uuid.New(), oauthCred(testOAuthConfig()))
	b := cache.Get(context.Background(), uuid.New(), oauthCred(testOAuthConfig()))

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

	before := cache.Get(context.Background(), id, oauthCred(leaked))
	after := cache.Get(context.Background(), id, oauthCred(rotated))

	if before == after {
		t.Fatal("a rotated refresh token still resolved to the TokenSource built from the retired credential")
	}

	// ...while an unchanged credential must still hit the cache, which is
	// the whole point of holding one.
	again := cache.Get(context.Background(), id, oauthCred(rotated))
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

	if cache.Get(context.Background(), id, oauthCred(original)) == cache.Get(context.Background(), id, oauthCred(rotated)) {
		t.Fatal("a rotated client secret still resolved to the previously cached TokenSource")
	}
}

func TestTokenSourceCache_DeleteEvicts(t *testing.T) {
	cache := NewTokenSourceCache()
	id := uuid.New()

	first := cache.Get(context.Background(), id, oauthCred(testOAuthConfig()))
	cache.Delete(id)
	second := cache.Get(context.Background(), id, oauthCred(testOAuthConfig()))

	if first == second {
		t.Fatal("Get after Delete returned the same TokenSource; expected a fresh one to be built")
	}
}

// ---- service-account credential ----

func TestNewTokenSource_ServiceAccountNoNetworkCall(t *testing.T) {
	ts := NewTokenSource(context.Background(), serviceAccountCred(t, serviceAccountKeyJSON()))
	if ts == nil {
		t.Fatal("NewTokenSource returned nil for a service-account credential")
	}
}

// TestNewTokenSource_NoCredential covers the branch ParseConfig makes
// unreachable but a hand-built Credential does not. Returning nil here
// would hand oauth2.NewClient a nil TokenSource that panics on first use,
// inside a tools/call dispatch where CLAUDE.md requires a failing call to
// end cleanly rather than as a panic or a raw Go error.
func TestNewTokenSource_NoCredential(t *testing.T) {
	ts := NewTokenSource(context.Background(), Credential{})
	if ts == nil {
		t.Fatal("NewTokenSource returned nil rather than an erroring TokenSource")
	}
	tok, err := ts.Token()
	if err == nil {
		t.Fatal("expected a credential-less TokenSource to fail on Token()")
	}
	if tok != nil {
		t.Fatal("expected no token alongside the error")
	}
}

// TestTokenSourceCache_RotatedServiceAccountKeyRebuilds is the
// service-account half of TestTokenSourceCache_RotatedCredentialRebuilds,
// and the regression the credential-variant refactor most easily
// introduces: a fingerprint left digesting only the three OAuth fields
// would hash every service-account credential identically, so a customer
// who rotated a leaked key would keep being served a TokenSource holding
// the retired one — a rotation that appears to succeed and changes nothing.
func TestTokenSourceCache_RotatedServiceAccountKeyRebuilds(t *testing.T) {
	cache := NewTokenSourceCache()
	id := uuid.New()

	leaked := serviceAccountCred(t, keyJSONWithPrivateKey(`-----BEGIN PRIVATE KEY-----\nleaked\n-----END PRIVATE KEY-----\n`))
	rotated := serviceAccountCred(t, keyJSONWithPrivateKey(`-----BEGIN PRIVATE KEY-----\nrotated\n-----END PRIVATE KEY-----\n`))

	before := cache.Get(context.Background(), id, leaked)
	after := cache.Get(context.Background(), id, rotated)
	if before == after {
		t.Fatal("a rotated service-account key still resolved to the TokenSource built from the retired one")
	}

	again := cache.Get(context.Background(), id, rotated)
	if again != after {
		t.Fatal("an unchanged service-account key rebuilt its TokenSource instead of reusing the cached one")
	}
}

// Switching a connector between credential variants must evict too — the
// case the variant tag in fingerprint exists for.
func TestTokenSourceCache_CredentialVariantSwitchRebuilds(t *testing.T) {
	cache := NewTokenSourceCache()
	id := uuid.New()

	viaOAuth := cache.Get(context.Background(), id, oauthCred(testOAuthConfig()))
	viaServiceAccount := cache.Get(context.Background(), id, serviceAccountCred(t, serviceAccountKeyJSON()))

	if viaOAuth == viaServiceAccount {
		t.Fatal("switching credential variant still resolved to the previously cached TokenSource")
	}
}

// The two variants must never fingerprint alike, independently of the cache.
func TestFingerprint_DistinguishesVariantsAndKeys(t *testing.T) {
	oauth := fingerprint(oauthCred(testOAuthConfig()))
	sa := fingerprint(serviceAccountCred(t, serviceAccountKeyJSON()))
	saRotated := fingerprint(serviceAccountCred(t, keyJSONWithPrivateKey(`-----BEGIN PRIVATE KEY-----\nother\n-----END PRIVATE KEY-----\n`)))
	none := fingerprint(Credential{})

	all := map[string][sha256.Size]byte{
		"oauth": oauth, "service_account": sa, "service_account rotated": saRotated, "empty": none,
	}
	for nameA, a := range all {
		for nameB, b := range all {
			if nameA != nameB && a == b {
				t.Fatalf("%s and %s fingerprint identically", nameA, nameB)
			}
		}
	}

	// ...and the same credential must fingerprint stably, or nothing would
	// ever hit the cache.
	if fingerprint(oauthCred(testOAuthConfig())) != oauth {
		t.Fatal("an unchanged oauth credential produced two different fingerprints")
	}
	if fingerprint(serviceAccountCred(t, serviceAccountKeyJSON())) != sa {
		t.Fatal("an unchanged service-account credential produced two different fingerprints")
	}
}
