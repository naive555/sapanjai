package middleware

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"github.com/sapanjai/backend/internal/infra/database/db"
)

// ---- hand-mocked dependencies ----

type mockMCPKeyLookup struct {
	getByHash        func(ctx context.Context, keyHash string) (db.GetMCPKeyByHashRow, error)
	stampLastUsed    func(ctx context.Context, id uuid.UUID) error
	stampCallCount   int
	lastStampedKeyID uuid.UUID
}

func (m *mockMCPKeyLookup) GetMCPKeyByHash(ctx context.Context, keyHash string) (db.GetMCPKeyByHashRow, error) {
	return m.getByHash(ctx, keyHash)
}

func (m *mockMCPKeyLookup) StampMCPKeyLastUsed(ctx context.Context, id uuid.UUID) error {
	m.stampCallCount++
	m.lastStampedKeyID = id
	if m.stampLastUsed != nil {
		return m.stampLastUsed(ctx, id)
	}
	return nil
}

// fakePrincipal is a minimal stand-in for *rbac.Principal, used only to
// prove RequireMCPKey threads resolve's return value onto the request
// context untouched — internal/middleware must not (cannot, without an
// import cycle — see MCPPrincipalResolver's doc comment) know the concrete
// rbac type.
type fakePrincipal struct {
	userID uuid.UUID
	scopes []string
}

func newResolver(t *testing.T, want *fakePrincipal) (MCPPrincipalResolver, *int) {
	t.Helper()
	calls := 0
	return func(ctx context.Context, userID, organizationID uuid.UUID, scopes []string) (any, error) {
		calls++
		if userID != want.userID {
			t.Errorf("resolve called with userID = %s, want %s", userID, want.userID)
		}
		return want, nil
	}, &calls
}

func validMCPKeyRow() db.GetMCPKeyByHashRow {
	return db.GetMCPKeyByHashRow{
		ID:             uuid.New(),
		OrganizationID: uuid.New(),
		UserID:         uuid.New(),
		Name:           "test-key",
		KeyHash:        hashMCPToken("sk_live_test-token"),
	}
}

// ---- RequireMCPKey: rejections ----

func TestRequireMCPKey_NoToken(t *testing.T) {
	store := &mockMCPKeyLookup{}
	resolve, _ := newResolver(t, &fakePrincipal{})
	c, rec := newTestContext(http.MethodPost, "/mcp/"+uuid.NewString(), nil)

	err := RequireMCPKey(store, resolve, nil)(okNext)(c)
	assertHTTPError(t, err, http.StatusUnauthorized, "Unauthorized")
	if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="sapanjai"` {
		t.Errorf("WWW-Authenticate = %q, want %q", got, `Bearer realm="sapanjai"`)
	}
}

func TestRequireMCPKey_MalformedAuthorizationHeader(t *testing.T) {
	store := &mockMCPKeyLookup{}
	resolve, _ := newResolver(t, &fakePrincipal{})
	c, _ := newTestContext(http.MethodPost, "/mcp/"+uuid.NewString(), map[string]string{
		"Authorization": "not-a-bearer-token",
	})

	err := RequireMCPKey(store, resolve, nil)(okNext)(c)
	assertHTTPError(t, err, http.StatusUnauthorized, "Unauthorized")
}

func TestRequireMCPKey_UnknownHash(t *testing.T) {
	store := &mockMCPKeyLookup{
		getByHash: func(ctx context.Context, keyHash string) (db.GetMCPKeyByHashRow, error) {
			return db.GetMCPKeyByHashRow{}, pgx.ErrNoRows
		},
	}
	resolve, _ := newResolver(t, &fakePrincipal{})
	c, rec := newTestContext(http.MethodPost, "/mcp/"+uuid.NewString(), map[string]string{
		"Authorization": "Bearer sk_live_bogus",
	})

	err := RequireMCPKey(store, resolve, nil)(okNext)(c)
	assertHTTPError(t, err, http.StatusUnauthorized, "Unauthorized")
	if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="sapanjai", error="invalid_token"` {
		t.Errorf("WWW-Authenticate = %q, want the invalid_token variant", got)
	}
}

func TestRequireMCPKey_StoreErrorPropagates(t *testing.T) {
	wantErr := errors.New("db is down")
	store := &mockMCPKeyLookup{
		getByHash: func(ctx context.Context, keyHash string) (db.GetMCPKeyByHashRow, error) {
			return db.GetMCPKeyByHashRow{}, wantErr
		},
	}
	resolve, _ := newResolver(t, &fakePrincipal{})
	c, _ := newTestContext(http.MethodPost, "/mcp/"+uuid.NewString(), map[string]string{
		"Authorization": "Bearer sk_live_bogus",
	})

	err := RequireMCPKey(store, resolve, nil)(okNext)(c)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v (a real infra error must not be swallowed into a 401)", err, wantErr)
	}
}

func TestRequireMCPKey_RevokedKey(t *testing.T) {
	row := validMCPKeyRow()
	row.RevokedAt = pgtype.Timestamp{Time: time.Now().Add(-time.Hour), Valid: true}
	store := &mockMCPKeyLookup{
		getByHash: func(ctx context.Context, keyHash string) (db.GetMCPKeyByHashRow, error) { return row, nil },
	}
	resolve, calls := newResolver(t, &fakePrincipal{})
	c, _ := newTestContext(http.MethodPost, "/mcp/"+uuid.NewString(), map[string]string{
		"Authorization": "Bearer sk_live_test-token",
	})

	err := RequireMCPKey(store, resolve, nil)(okNext)(c)
	assertHTTPError(t, err, http.StatusUnauthorized, "Unauthorized")
	if *calls != 0 {
		t.Errorf("resolve called %d times, want 0 for a revoked key", *calls)
	}
}

func TestRequireMCPKey_ExpiredKey(t *testing.T) {
	row := validMCPKeyRow()
	row.ExpiresAt = pgtype.Timestamp{Time: time.Now().Add(-time.Minute), Valid: true}
	store := &mockMCPKeyLookup{
		getByHash: func(ctx context.Context, keyHash string) (db.GetMCPKeyByHashRow, error) { return row, nil },
	}
	resolve, calls := newResolver(t, &fakePrincipal{})
	c, _ := newTestContext(http.MethodPost, "/mcp/"+uuid.NewString(), map[string]string{
		"Authorization": "Bearer sk_live_test-token",
	})

	err := RequireMCPKey(store, resolve, nil)(okNext)(c)
	assertHTTPError(t, err, http.StatusUnauthorized, "Unauthorized")
	if *calls != 0 {
		t.Errorf("resolve called %d times, want 0 for an expired key", *calls)
	}
}

func TestRequireMCPKey_BannedOwner(t *testing.T) {
	// An MCP PAT has no expiry of its own, so a banned owner's key would
	// otherwise authenticate forever — the strictly worse hole
	// docs/11-admin-panel.md §4 calls out. The rejection must be the same
	// indistinguishable 401 as every other credential failure: no
	// "account suspended" message, no distinct WWW-Authenticate error.
	row := validMCPKeyRow()
	row.OwnerBannedAt = pgtype.Timestamp{Time: time.Now().Add(-time.Hour), Valid: true}
	store := &mockMCPKeyLookup{
		getByHash: func(ctx context.Context, keyHash string) (db.GetMCPKeyByHashRow, error) { return row, nil },
	}
	resolve, calls := newResolver(t, &fakePrincipal{})
	c, rec := newTestContext(http.MethodPost, "/mcp/"+uuid.NewString(), map[string]string{
		"Authorization": "Bearer sk_live_test-token",
	})

	err := RequireMCPKey(store, resolve, nil)(okNext)(c)
	assertHTTPError(t, err, http.StatusUnauthorized, "Unauthorized")
	if *calls != 0 {
		t.Errorf("resolve called %d times, want 0 for a banned owner", *calls)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="sapanjai", error="invalid_token"` {
		t.Errorf("WWW-Authenticate = %q, want the same invalid_token variant as a revoked/expired/unknown key", got)
	}
}

func TestRequireMCPKey_FutureExpiryIsAccepted(t *testing.T) {
	row := validMCPKeyRow()
	row.ExpiresAt = pgtype.Timestamp{Time: time.Now().Add(time.Hour), Valid: true}
	store := &mockMCPKeyLookup{
		getByHash: func(ctx context.Context, keyHash string) (db.GetMCPKeyByHashRow, error) { return row, nil },
	}
	want := &fakePrincipal{userID: row.UserID}
	resolve, calls := newResolver(t, want)
	c, _ := newTestContext(http.MethodPost, "/mcp/"+uuid.NewString(), map[string]string{
		"Authorization": "Bearer sk_live_test-token",
	})

	var gotFromContext any
	err := RequireMCPKey(store, resolve, nil)(func(c echo.Context) error {
		gotFromContext, _ = MCPPrincipalFromContext(c.Request().Context())
		return nil
	})(c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if *calls != 1 {
		t.Errorf("resolve called %d times, want 1", *calls)
	}
	if gotFromContext != any(want) {
		t.Errorf("principal on context = %#v, want %#v", gotFromContext, want)
	}
}

// ---- RequireMCPKey: happy path wiring ----

func TestRequireMCPKey_HappyPath_PrincipalOnRequestContext(t *testing.T) {
	row := validMCPKeyRow()
	store := &mockMCPKeyLookup{
		getByHash: func(ctx context.Context, keyHash string) (db.GetMCPKeyByHashRow, error) {
			if keyHash != hashMCPToken("sk_live_test-token") {
				t.Errorf("looked up hash %q, want the sha256 of the presented token", keyHash)
			}
			return row, nil
		},
	}
	want := &fakePrincipal{userID: row.UserID}
	resolve, calls := newResolver(t, want)
	c, _ := newTestContext(http.MethodPost, "/mcp/"+uuid.NewString(), map[string]string{
		"Authorization": "Bearer sk_live_test-token",
	})

	var gotOnRequestContext any
	var sawOnEchoContext bool
	next := func(c echo.Context) error {
		// The load-bearing assertion: the principal must be reachable via
		// c.Request().Context() (what the SDK's http.Handler sees), not
		// merely via c.Set() (which the SDK cannot see at all).
		gotOnRequestContext, sawOnEchoContext = MCPPrincipalFromContext(c.Request().Context())
		return c.NoContent(http.StatusOK)
	}

	err := RequireMCPKey(store, resolve, nil)(next)(c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !sawOnEchoContext {
		t.Fatal("principal not found on c.Request().Context() inside next")
	}
	if gotOnRequestContext != any(want) {
		t.Errorf("principal = %#v, want %#v", gotOnRequestContext, want)
	}
	if *calls != 1 {
		t.Errorf("resolve called %d times, want 1", *calls)
	}
	if store.stampCallCount != 1 {
		t.Errorf("StampMCPKeyLastUsed called %d times, want 1", store.stampCallCount)
	}
	if store.lastStampedKeyID != row.ID {
		t.Errorf("stamped key id = %s, want %s", store.lastStampedKeyID, row.ID)
	}
}

func TestRequireMCPKey_StampFailureNeverFailsTheRequest(t *testing.T) {
	row := validMCPKeyRow()
	store := &mockMCPKeyLookup{
		getByHash:     func(ctx context.Context, keyHash string) (db.GetMCPKeyByHashRow, error) { return row, nil },
		stampLastUsed: func(ctx context.Context, id uuid.UUID) error { return errors.New("stamp write failed") },
	}
	resolve, _ := newResolver(t, &fakePrincipal{userID: row.UserID})
	c, _ := newTestContext(http.MethodPost, "/mcp/"+uuid.NewString(), map[string]string{
		"Authorization": "Bearer sk_live_test-token",
	})

	err := RequireMCPKey(store, resolve, nil)(okNext)(c)
	if err != nil {
		t.Fatalf("a best-effort last_used_at stamp failure must not fail the request: %v", err)
	}
}

func TestRequireMCPKey_ResolveErrorPropagates(t *testing.T) {
	row := validMCPKeyRow()
	store := &mockMCPKeyLookup{
		getByHash: func(ctx context.Context, keyHash string) (db.GetMCPKeyByHashRow, error) { return row, nil },
	}
	wantErr := errors.New("rbac lookup failed")
	resolve := func(ctx context.Context, userID, organizationID uuid.UUID, scopes []string) (any, error) {
		return nil, wantErr
	}
	c, _ := newTestContext(http.MethodPost, "/mcp/"+uuid.NewString(), map[string]string{
		"Authorization": "Bearer sk_live_test-token",
	})

	err := RequireMCPKey(store, resolve, nil)(okNext)(c)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if store.stampCallCount != 0 {
		t.Errorf("StampMCPKeyLastUsed called after a failed resolve, want 0 calls")
	}
}

func TestRequireMCPKey_ScopesPassedThroughToResolver(t *testing.T) {
	row := validMCPKeyRow()
	row.Scopes = []string{"connector:read"}
	store := &mockMCPKeyLookup{
		getByHash: func(ctx context.Context, keyHash string) (db.GetMCPKeyByHashRow, error) { return row, nil },
	}

	var gotScopes []string
	resolve := func(ctx context.Context, userID, organizationID uuid.UUID, scopes []string) (any, error) {
		gotScopes = scopes
		return &fakePrincipal{userID: userID, scopes: scopes}, nil
	}
	c, _ := newTestContext(http.MethodPost, "/mcp/"+uuid.NewString(), map[string]string{
		"Authorization": "Bearer sk_live_test-token",
	})

	err := RequireMCPKey(store, resolve, nil)(okNext)(c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gotScopes) != 1 || gotScopes[0] != "connector:read" {
		t.Errorf("scopes passed to resolve = %v, want [connector:read]", gotScopes)
	}
}

// ---- key name on context (gateway-core step 3: sapanjai_whoami) ----

func TestRequireMCPKey_HappyPath_KeyNameOnRequestContext(t *testing.T) {
	row := validMCPKeyRow() // Name: "test-key"
	store := &mockMCPKeyLookup{
		getByHash: func(ctx context.Context, keyHash string) (db.GetMCPKeyByHashRow, error) { return row, nil },
	}
	resolve, _ := newResolver(t, &fakePrincipal{userID: row.UserID})
	c, _ := newTestContext(http.MethodPost, "/mcp/"+uuid.NewString(), map[string]string{
		"Authorization": "Bearer sk_live_test-token",
	})

	var gotKeyName string
	var sawKeyName bool
	next := func(c echo.Context) error {
		gotKeyName, sawKeyName = MCPKeyNameFromContext(c.Request().Context())
		return c.NoContent(http.StatusOK)
	}

	err := RequireMCPKey(store, resolve, nil)(next)(c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !sawKeyName {
		t.Fatal("key name not found on c.Request().Context() inside next")
	}
	if gotKeyName != row.Name {
		t.Errorf("key name = %q, want %q", gotKeyName, row.Name)
	}
}

func TestMCPKeyNameFromContext_AbsentWhenNeverAuthenticated(t *testing.T) {
	name, ok := MCPKeyNameFromContext(context.Background())
	if ok {
		t.Errorf("ok = true for a context that never went through RequireMCPKey, want false")
	}
	if name != "" {
		t.Errorf("name = %q, want empty string when absent", name)
	}
}
