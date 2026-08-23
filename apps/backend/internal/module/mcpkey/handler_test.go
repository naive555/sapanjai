package mcpkey

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"github.com/sapanjai/backend/internal/infra/database/db"
	appmw "github.com/sapanjai/backend/internal/middleware"
)

// testPermActionPattern mirrors internal/server/validator.go's unexported
// permActionPattern (that package can't be imported here without a cycle),
// so CreateRequest.Scopes' "permaction" tag has something registered to
// find — an unregistered tag panics at struct-cache time regardless of
// whether any request in a given test actually sets Scopes.
var testPermActionPattern = regexp.MustCompile(`^(\*|[a-z][a-z0-9_]*:(\*|[a-z][a-z0-9_]*))$`)

// testValidator adapts go-playground/validator to echo.Validator, mirroring
// internal/server/validator.go's requestValidator so CreateRequest's struct
// tags are actually exercised here — the "connectortype"-style custom tag
// isn't needed by this module, so "permaction" is the only custom tag
// registered.
type testValidator struct{ v *validator.Validate }

func newTestValidator() *testValidator {
	v := validator.New(validator.WithRequiredStructEnabled())
	_ = v.RegisterValidation("permaction", func(fl validator.FieldLevel) bool {
		return testPermActionPattern.MatchString(fl.Field().String())
	})
	return &testValidator{v: v}
}

func (tv *testValidator) Validate(i any) error {
	return tv.v.Struct(i)
}

// ---- hand-mocked appmw.Guards dependencies (mirrors connector's
// handler_test.go — these satisfy the unexported interfaces appmw.NewGuards
// takes structurally, without any real token/DB/Redis infra). ----

type fakeTokenVerifier struct {
	userID uuid.UUID
}

func (f *fakeTokenVerifier) VerifyAccessToken(token string) (uuid.UUID, string, error) {
	return f.userID, "caller@example.com", nil
}

type fakeBlacklistChecker struct{}

func (f *fakeBlacklistChecker) IsBlacklisted(ctx context.Context, token string) (bool, error) {
	return false, nil
}

type fakeMembershipStore struct{}

func (f *fakeMembershipStore) GetMembership(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error) {
	return db.Membership{ID: uuid.New(), UserID: arg.UserID, OrganizationID: arg.OrganizationID, Role: "member"}, nil
}

type fakePermissionChecker struct {
	granted map[string]bool
}

func (f *fakePermissionChecker) HasPermission(ctx context.Context, userID, organizationID uuid.UUID, action string) (bool, error) {
	return f.granted[action], nil
}

func newTestGuards(userID uuid.UUID, granted map[string]bool) *appmw.Guards {
	return appmw.NewGuards(
		&fakeTokenVerifier{userID: userID},
		&fakeBlacklistChecker{},
		&fakeMembershipStore{},
		&fakePermissionChecker{granted: granted},
	)
}

// newTestHandler builds a Handler backed by a real Service over the given
// store. Denial-path tests pass a bare &mockMCPKeyStore{} (every func field
// nil): if RequirePermission's isolation were ever broken and a denied
// request reached the service anyway, the nil func call panics and fails
// the test loudly instead of silently passing.
func newTestHandler(store mcpKeyStore) *Handler {
	svc := NewService(store, newTestLog())
	return NewHandler(svc)
}

func newTestServer(handler *Handler, guards *appmw.Guards) *echo.Echo {
	e := echo.New()
	e.Validator = newTestValidator()
	handler.Register(e.Group("/mcp-keys"), guards)
	return e
}

func doMCPKeyRequest(t *testing.T, e *echo.Echo, method, path string, headers map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	var body map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response body: %v (body: %s)", err, rec.Body.String())
		}
	}
	return rec, body
}

// ---- permission enforcement ----

func TestHandler_ReadGranted_WriteDenied(t *testing.T) {
	orgID := uuid.New()
	userID := uuid.New()
	store := &mockMCPKeyStore{
		listMCPKeysByOrg: func(ctx context.Context, organizationID uuid.UUID) ([]db.McpApiKey, error) {
			return nil, nil
		},
	}
	guards := newTestGuards(userID, map[string]bool{PermissionRead: true})
	e := newTestServer(newTestHandler(store), guards)

	headers := map[string]string{
		"Authorization":     "Bearer x",
		"x-organization-id": orgID.String(),
	}

	req := httptest.NewRequest(http.MethodGet, "/mcp-keys", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /mcp-keys: status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	rec2, body2 := doMCPKeyRequest(t, e, http.MethodPost, "/mcp-keys", headers)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("POST /mcp-keys: status = %d, want 403; body = %v", rec2.Code, body2)
	}
	if body2["message"] != "Missing permission: "+PermissionWrite {
		t.Fatalf("message = %v, want %q", body2["message"], "Missing permission: "+PermissionWrite)
	}
}

func TestHandler_NoPermissions_EveryRouteDenied(t *testing.T) {
	orgID := uuid.New()
	userID := uuid.New()
	// Every func field is nil: if a denied request ever reached the
	// service, calling any of these panics and fails the test.
	store := &mockMCPKeyStore{}
	guards := newTestGuards(userID, map[string]bool{})
	e := newTestServer(newTestHandler(store), guards)

	headers := map[string]string{
		"Authorization":     "Bearer x",
		"x-organization-id": orgID.String(),
	}
	keyID := uuid.NewString()

	cases := []struct {
		method, path, wantAction string
	}{
		{http.MethodPost, "/mcp-keys", PermissionWrite},
		{http.MethodGet, "/mcp-keys", PermissionRead},
		{http.MethodDelete, "/mcp-keys/" + keyID, PermissionDelete},
	}

	for _, tc := range cases {
		rec, body := doMCPKeyRequest(t, e, tc.method, tc.path, headers)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: status = %d, want 403; body = %v", tc.method, tc.path, rec.Code, body)
			continue
		}
		want := "Missing permission: " + tc.wantAction
		if body["message"] != want {
			t.Errorf("%s %s: message = %v, want %q", tc.method, tc.path, body["message"], want)
		}
	}
}

func TestHandler_MissingOrgHeader(t *testing.T) {
	userID := uuid.New()
	store := &mockMCPKeyStore{}
	guards := newTestGuards(userID, map[string]bool{PermissionRead: true})
	e := newTestServer(newTestHandler(store), guards)

	headers := map[string]string{"Authorization": "Bearer x"} // no x-organization-id

	rec, body := doMCPKeyRequest(t, e, http.MethodGet, "/mcp-keys", headers)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %v", rec.Code, body)
	}
	if body["message"] != "Missing x-organization-id header" {
		t.Fatalf("message = %v, want %q", body["message"], "Missing x-organization-id header")
	}
}

// ---- response shape ----

func TestHandler_List_NeverIncludesKeyHashOrToken(t *testing.T) {
	orgID := uuid.New()
	userID := uuid.New()
	store := &mockMCPKeyStore{
		listMCPKeysByOrg: func(ctx context.Context, organizationID uuid.UUID) ([]db.McpApiKey, error) {
			return []db.McpApiKey{
				{ID: uuid.New(), OrganizationID: orgID, UserID: userID, Name: "laptop", KeyHash: "deadbeef"},
			}, nil
		},
	}
	guards := newTestGuards(userID, map[string]bool{PermissionRead: true})
	e := newTestServer(newTestHandler(store), guards)

	headers := map[string]string{
		"Authorization":     "Bearer x",
		"x-organization-id": orgID.String(),
	}

	req := httptest.NewRequest(http.MethodGet, "/mcp-keys", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %v, want 1 entry", rows)
	}
	for _, key := range []string{"keyHash", "key_hash", "apiKey"} {
		if _, ok := rows[0][key]; ok {
			t.Fatalf("list response leaks a %q key: %v", key, rows[0])
		}
	}
	if rows[0]["name"] != "laptop" {
		t.Errorf("name = %v, want %q", rows[0]["name"], "laptop")
	}
}

func TestHandler_Create_ReturnsAPIKeyOnce(t *testing.T) {
	orgID := uuid.New()
	userID := uuid.New()
	store := &mockMCPKeyStore{
		getMCPKeyByName: func(ctx context.Context, arg db.GetMCPKeyByNameParams) (db.McpApiKey, error) {
			return db.McpApiKey{}, pgx.ErrNoRows
		},
		createMCPKey: func(ctx context.Context, arg db.CreateMCPKeyParams) (db.McpApiKey, error) {
			return db.McpApiKey{ID: uuid.New(), OrganizationID: arg.OrganizationID, UserID: arg.UserID, Name: arg.Name, KeyHash: arg.KeyHash}, nil
		},
	}
	guards := newTestGuards(userID, map[string]bool{PermissionWrite: true})
	e := newTestServer(newTestHandler(store), guards)

	headers := map[string]string{
		"Authorization":     "Bearer x",
		"x-organization-id": orgID.String(),
		"Content-Type":      "application/json",
	}

	req := httptest.NewRequest(http.MethodPost, "/mcp-keys", strings.NewReader(`{"name":"laptop"}`))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	apiKey, _ := body["apiKey"].(string)
	if apiKey == "" {
		t.Fatalf("create response missing apiKey: %v", body)
	}
	if body["name"] != "laptop" {
		t.Errorf("name = %v, want %q", body["name"], "laptop")
	}
}

func TestHandler_Create_ValidationFailure(t *testing.T) {
	orgID := uuid.New()
	userID := uuid.New()
	store := &mockMCPKeyStore{} // must not be reached
	guards := newTestGuards(userID, map[string]bool{PermissionWrite: true})
	e := newTestServer(newTestHandler(store), guards)

	headers := map[string]string{
		"Authorization":     "Bearer x",
		"x-organization-id": orgID.String(),
		"Content-Type":      "application/json",
	}

	req := httptest.NewRequest(http.MethodPost, "/mcp-keys", strings.NewReader(`{"name":""}`))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", rec.Code, rec.Body.String())
	}
}
