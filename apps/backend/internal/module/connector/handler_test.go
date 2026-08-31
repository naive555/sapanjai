package connector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/sapanjai/backend/internal/infra/database/db"
	appmw "github.com/sapanjai/backend/internal/middleware"
	"github.com/sapanjai/backend/internal/shared/authtoken"
)

// ---- hand-mocked appmw.Guards dependencies ----
//
// These satisfy the unexported interfaces appmw.NewGuards takes
// (tokenVerifier, blacklistChecker, membershipStore, permissionChecker)
// structurally — Go interface satisfaction doesn't require importing the
// interface type, only matching its method set. This lets a route-level
// test drive the real RequirePermission middleware without any real
// token/DB/Redis infra.

type fakeTokenVerifier struct {
	userID uuid.UUID
}

func (f *fakeTokenVerifier) VerifyAccessToken(token string) (authtoken.AccessToken, error) {
	return authtoken.AccessToken{UserID: f.userID, Email: "caller@example.com"}, nil
}

type fakeBlacklistChecker struct{}

func (f *fakeBlacklistChecker) IsBlacklisted(ctx context.Context, token string) (bool, error) {
	return false, nil
}

func (f *fakeBlacklistChecker) IsBanned(ctx context.Context, userID uuid.UUID) (bool, error) {
	return false, nil
}

type fakeMembershipStore struct{}

func (f *fakeMembershipStore) GetMembership(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error) {
	return db.Membership{ID: uuid.New(), UserID: arg.UserID, OrganizationID: arg.OrganizationID, Role: "member"}, nil
}

func (f *fakeMembershipStore) GetUserByID(ctx context.Context, id uuid.UUID) (db.User, error) {
	return db.User{ID: id}, nil
}

// fakePermissionChecker grants exactly the actions listed in granted,
// mirroring rbac.Service.HasPermission's signature without touching RBAC or
// a database — the real permission-string semantics (*, resource:*) are
// exercised separately in the rbac package and in the connector integration
// test.
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
// store. Denial-path tests pass a bare &mockConnectorStore{} (every func
// field nil): if RequirePermission's isolation were ever broken and a
// denied request reached the service anyway, the nil func call panics and
// fails the test loudly instead of silently passing.
func newTestHandler(t *testing.T, store connectorStore) *Handler {
	t.Helper()
	svc := NewService(store, newTestCrypto(t), newTestAudit(&spyQuerier{}), allowAllLimiter(), NewRegistry(), newTestLog())
	return NewHandler(svc)
}

func newTestServer(handler *Handler, guards *appmw.Guards) *echo.Echo {
	e := echo.New()
	handler.Register(e.Group("/connectors"), guards)
	return e
}

// doConnectorRequest issues method/path against e with the given headers and
// decodes the JSON response body into a map (nil body decodes to a nil map,
// which is fine for the assertions below — none of them touch an empty
// body).
func doConnectorRequest(t *testing.T, e *echo.Echo, method, path string, headers map[string]string) (*httptest.ResponseRecorder, map[string]any) {
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
	store := &mockConnectorStore{
		listConnectorsByOrg: func(ctx context.Context, organizationID uuid.UUID) ([]db.Connector, error) {
			return nil, nil
		},
	}
	guards := newTestGuards(userID, map[string]bool{PermissionRead: true})
	e := newTestServer(newTestHandler(t, store), guards)

	headers := map[string]string{
		"Authorization":     "Bearer x",
		"x-organization-id": orgID.String(),
	}

	req := httptest.NewRequest(http.MethodGet, "/connectors", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /connectors: status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	rec2, body2 := doConnectorRequest(t, e, http.MethodPost, "/connectors", headers)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("POST /connectors: status = %d, want 403; body = %v", rec2.Code, body2)
	}
	if body2["message"] != "Missing permission: "+PermissionWrite {
		t.Fatalf("message = %v, want %q", body2["message"], "Missing permission: "+PermissionWrite)
	}
}

func TestHandler_WriteGranted_DeleteDenied(t *testing.T) {
	orgID := uuid.New()
	userID := uuid.New()
	store := &mockConnectorStore{} // DELETE must never reach it
	guards := newTestGuards(userID, map[string]bool{PermissionWrite: true})
	e := newTestServer(newTestHandler(t, store), guards)

	headers := map[string]string{
		"Authorization":     "Bearer x",
		"x-organization-id": orgID.String(),
	}

	rec, body := doConnectorRequest(t, e, http.MethodDelete, "/connectors/"+uuid.NewString(), headers)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("DELETE: status = %d, want 403; body = %v", rec.Code, body)
	}
	if body["message"] != "Missing permission: "+PermissionDelete {
		t.Fatalf("message = %v, want %q", body["message"], "Missing permission: "+PermissionDelete)
	}
}

func TestHandler_NoPermissions_EveryRouteDenied(t *testing.T) {
	orgID := uuid.New()
	userID := uuid.New()
	// Every func field is nil: if a denied request ever reached the
	// service, calling any of these panics and fails the test.
	store := &mockConnectorStore{}
	guards := newTestGuards(userID, map[string]bool{})
	e := newTestServer(newTestHandler(t, store), guards)

	headers := map[string]string{
		"Authorization":     "Bearer x",
		"x-organization-id": orgID.String(),
	}
	connID := uuid.NewString()

	cases := []struct {
		method, path, wantAction string
	}{
		{http.MethodPost, "/connectors", PermissionWrite},
		{http.MethodGet, "/connectors", PermissionRead},
		{http.MethodGet, "/connectors/" + connID, PermissionRead},
		{http.MethodPatch, "/connectors/" + connID, PermissionWrite},
		{http.MethodDelete, "/connectors/" + connID, PermissionDelete},
		{http.MethodPost, "/connectors/" + connID + "/health-check", PermissionWrite},
	}

	for _, tc := range cases {
		rec, body := doConnectorRequest(t, e, tc.method, tc.path, headers)
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
	store := &mockConnectorStore{}
	guards := newTestGuards(userID, map[string]bool{PermissionRead: true})
	e := newTestServer(newTestHandler(t, store), guards)

	headers := map[string]string{"Authorization": "Bearer x"} // no x-organization-id

	rec, body := doConnectorRequest(t, e, http.MethodGet, "/connectors", headers)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %v", rec.Code, body)
	}
	if body["message"] != "Missing x-organization-id header" {
		t.Fatalf("message = %v, want %q", body["message"], "Missing x-organization-id header")
	}
}

// ---- response shape ----

func TestHandler_Get_ResponseNeverIncludesConfig(t *testing.T) {
	orgID := uuid.New()
	userID := uuid.New()
	connID := uuid.New()
	store := &mockConnectorStore{
		getConnector: func(ctx context.Context, arg db.GetConnectorParams) (db.Connector, error) {
			return db.Connector{
				ID:              connID,
				OrganizationID:  orgID,
				Name:            "warehouse-db",
				Type:            string(TypeGeneric),
				Status:          "inactive",
				EncryptedConfig: []byte(`{"v":1,"kid":"env:x","dek":"AA==","ct":"AA=="}`),
			}, nil
		},
	}
	guards := newTestGuards(userID, map[string]bool{PermissionRead: true})
	e := newTestServer(newTestHandler(t, store), guards)

	headers := map[string]string{
		"Authorization":     "Bearer x",
		"x-organization-id": orgID.String(),
	}

	rec, body := doConnectorRequest(t, e, http.MethodGet, "/connectors/"+connID.String(), headers)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %v", rec.Code, body)
	}
	if _, ok := body["config"]; ok {
		t.Fatalf("response leaks a config key: %v", body)
	}
	if _, ok := body["encryptedConfig"]; ok {
		t.Fatalf("response leaks an encryptedConfig key: %v", body)
	}
	if body["name"] != "warehouse-db" {
		t.Errorf("name = %v, want %q", body["name"], "warehouse-db")
	}
	if body["lastHealthCheckAt"] != nil {
		t.Errorf("lastHealthCheckAt = %v, want nil", body["lastHealthCheckAt"])
	}
}
