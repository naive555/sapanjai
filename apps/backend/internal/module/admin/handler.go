package admin

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	appmw "github.com/sapanjai/backend/internal/middleware"
	"github.com/sapanjai/backend/internal/shared/apperror"
	"github.com/sapanjai/backend/internal/shared/httpx"
)

// Handler implements the /admin read routes (execution plan Phase 2). All
// of it is guarded by RequirePlatformRole("superadmin", "support") — see
// docs/11-admin-panel.md §4's role matrix: every route this phase builds
// is a read, and support reads everything.
type Handler struct {
	service *Service
}

// NewHandler builds an admin Handler.
func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// Register mounts every Phase 2 /admin route under g, all behind one
// RequirePlatformRole instance shared across the group. The superadmin-only
// mutation routes from docs/11-admin-panel.md §4 (plan/limit changes,
// ban, platform-role grant, delete-org, impersonate) are later phases and
// are not registered here.
func (h *Handler) Register(g *echo.Group, guards *appmw.Guards) {
	read := guards.RequirePlatformRole("superadmin", "support")

	g.GET("/me", h.me, read)
	g.GET("/organizations", h.listOrganizations, read)
	g.GET("/organizations/:orgId", h.getOrganization, read)
	g.GET("/users", h.listUsers, read)
	g.GET("/users/:userId", h.getUser, read)
	g.GET("/connectors", h.listConnectors, read)
	g.GET("/mcp-keys", h.listMCPKeys, read)
	g.GET("/audit-logs", h.queryAuditLogs, read)
	g.GET("/system/stats", h.systemStats, read)
	g.GET("/plans", h.listPlans, read)
}

// me returns the calling staff account's own admin-facing profile.
// @Summary  Get the calling staff account's admin profile
// @Tags     admin
// @Security BearerAuth
// @Produce  json
// @Success  200  {object}  MeResponse
// @Failure  401  {object}  httpx.ErrorResponse  "Unauthorized"
// @Failure  403  {object}  httpx.ErrorResponse  "Insufficient permissions"
// @Router   /admin/me [get]
func (h *Handler) me(c echo.Context) error {
	resp, err := h.service.Me(c.Request().Context(), appmw.UserID(c))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp)
}

// listOrganizations returns a page of organizations across every tenant.
// @Summary  List organizations (cross-org)
// @Tags     admin
// @Security BearerAuth
// @Produce  json
// @Param    search  query     string  false  "Match against name or slug"
// @Param    limit   query     int     false  "Max results (1-100, default 50)"
// @Param    offset  query     int     false  "Pagination offset (default 0)"
// @Success  200     {object}  OrganizationsListResponse
// @Failure  401     {object}  httpx.ErrorResponse  "Unauthorized"
// @Failure  403     {object}  httpx.ErrorResponse  "Insufficient permissions"
// @Failure  422     {object}  httpx.ErrorResponse  "Validation failed"
// @Router   /admin/organizations [get]
func (h *Handler) listOrganizations(c echo.Context) error {
	var q OrganizationsQuery
	if err := httpx.BindAndValidate(c, &q); err != nil {
		return err
	}

	resp, err := h.service.ListOrganizations(c.Request().Context(), q.searchFilter(), q.limit(), q.offset())
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp)
}

// getOrganization returns one organization's detail view.
// @Summary  Get one organization's detail view (cross-org)
// @Tags     admin
// @Security BearerAuth
// @Produce  json
// @Param    orgId  path      string  true  "Organization ID"
// @Success  200    {object}  OrganizationDetailResponse
// @Failure  401    {object}  httpx.ErrorResponse  "Unauthorized"
// @Failure  403    {object}  httpx.ErrorResponse  "Insufficient permissions"
// @Failure  404    {object}  httpx.ErrorResponse  "Resource not found"
// @Router   /admin/organizations/{orgId} [get]
func (h *Handler) getOrganization(c echo.Context) error {
	id, err := orgIDParam(c)
	if err != nil {
		return err
	}

	resp, err := h.service.GetOrganization(c.Request().Context(), id)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp)
}

// listUsers returns a page of users across every tenant.
// @Summary  List users (cross-org)
// @Tags     admin
// @Security BearerAuth
// @Produce  json
// @Param    search  query     string  false  "Match against email or display name"
// @Param    role    query     string  false  "Filter by platform role: superadmin, support, or none"
// @Param    banned  query     bool    false  "Filter by ban status"
// @Param    limit   query     int     false  "Max results (1-100, default 50)"
// @Param    offset  query     int     false  "Pagination offset (default 0)"
// @Success  200     {object}  UsersListResponse
// @Failure  401     {object}  httpx.ErrorResponse  "Unauthorized"
// @Failure  403     {object}  httpx.ErrorResponse  "Insufficient permissions"
// @Failure  422     {object}  httpx.ErrorResponse  "Validation failed"
// @Router   /admin/users [get]
func (h *Handler) listUsers(c echo.Context) error {
	var q UsersQuery
	if err := httpx.BindAndValidate(c, &q); err != nil {
		return err
	}

	resp, err := h.service.ListUsers(c.Request().Context(), q.searchFilter(), q.Role, q.Banned, q.limit(), q.offset())
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp)
}

// getUser returns one user's detail view.
// @Summary  Get one user's detail view (cross-org)
// @Tags     admin
// @Security BearerAuth
// @Produce  json
// @Param    userId  path      string  true  "User ID"
// @Success  200     {object}  UserDetailResponse
// @Failure  401     {object}  httpx.ErrorResponse  "Unauthorized"
// @Failure  403     {object}  httpx.ErrorResponse  "Insufficient permissions"
// @Failure  404     {object}  httpx.ErrorResponse  "User not found"
// @Router   /admin/users/{userId} [get]
func (h *Handler) getUser(c echo.Context) error {
	id, err := userIDParam(c)
	if err != nil {
		return err
	}

	resp, err := h.service.GetUser(c.Request().Context(), id)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp)
}

// listConnectors returns a page of connector metadata across every
// tenant. Never encrypted_config, never decrypted config, not even a
// "config present" boolean (docs/11-admin-panel.md §7).
// @Summary  List connectors (cross-org, metadata only)
// @Tags     admin
// @Security BearerAuth
// @Produce  json
// @Param    organizationId  query     string  false  "Filter by organization ID"
// @Param    type            query     string  false  "Filter by connector type"
// @Param    status          query     string  false  "Filter by status: active, inactive, error"
// @Param    search          query     string  false  "Match against connector name"
// @Param    limit           query     int     false  "Max results (1-100, default 50)"
// @Param    offset          query     int     false  "Pagination offset (default 0)"
// @Success  200             {object}  ConnectorsListResponse
// @Failure  401             {object}  httpx.ErrorResponse  "Unauthorized"
// @Failure  403             {object}  httpx.ErrorResponse  "Insufficient permissions"
// @Failure  422             {object}  httpx.ErrorResponse  "Validation failed"
// @Router   /admin/connectors [get]
func (h *Handler) listConnectors(c echo.Context) error {
	var q ConnectorsQuery
	if err := httpx.BindAndValidate(c, &q); err != nil {
		return err
	}

	orgID, err := parseOptionalUUID(q.OrganizationID)
	if err != nil {
		return err
	}

	resp, err := h.service.ListConnectors(c.Request().Context(), orgID, q.Type, q.Status, q.searchFilter(), q.limit(), q.offset())
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp)
}

// listMCPKeys returns a page of MCP key metadata across every tenant.
// Never key_hash, never a raw token (docs/11-admin-panel.md §7).
// @Summary  List MCP keys (cross-org, metadata only)
// @Tags     admin
// @Security BearerAuth
// @Produce  json
// @Param    organizationId  query     string  false  "Filter by organization ID"
// @Param    userId          query     string  false  "Filter by owner user ID"
// @Param    search          query     string  false  "Match against key name or owner email"
// @Param    limit           query     int     false  "Max results (1-100, default 50)"
// @Param    offset          query     int     false  "Pagination offset (default 0)"
// @Success  200             {object}  MCPKeysListResponse
// @Failure  401             {object}  httpx.ErrorResponse  "Unauthorized"
// @Failure  403             {object}  httpx.ErrorResponse  "Insufficient permissions"
// @Failure  422             {object}  httpx.ErrorResponse  "Validation failed"
// @Router   /admin/mcp-keys [get]
func (h *Handler) listMCPKeys(c echo.Context) error {
	var q MCPKeysQuery
	if err := httpx.BindAndValidate(c, &q); err != nil {
		return err
	}

	orgID, err := parseOptionalUUID(q.OrganizationID)
	if err != nil {
		return err
	}
	userID, err := parseOptionalUUID(q.UserID)
	if err != nil {
		return err
	}

	resp, err := h.service.ListMCPKeys(c.Request().Context(), orgID, userID, q.searchFilter(), q.limit(), q.offset())
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp)
}

// queryAuditLogs returns a page of audit log entries across every tenant.
// action is repeatable and a trailing '*' means prefix match, so
// "?action=admin.*" matches every admin.* action. from/to are RFC3339,
// normalized to UTC before binding — audit_logs.created_at is a `timestamp`
// column with no time zone (the same trap QueryAuditLogs's own comment
// documents for the tenant-side GET /audit-logs).
// @Summary  Query audit logs (cross-org)
// @Tags     admin
// @Security BearerAuth
// @Produce  json
// @Param    organizationId  query     string    false  "Filter by organization ID"
// @Param    userId          query     string    false  "Filter by user ID"
// @Param    action          query     []string  false  "Filter by action; repeatable, trailing '*' means prefix match"  collectionFormat(multi)
// @Param    from            query     string    false  "Only logs at or after this RFC3339 timestamp"
// @Param    to              query     string    false  "Only logs at or before this RFC3339 timestamp"
// @Param    limit           query     int       false  "Max results (1-200, default 50)"
// @Param    offset          query     int       false  "Pagination offset (default 0)"
// @Success  200             {object}  AuditLogsListResponse
// @Failure  401             {object}  httpx.ErrorResponse  "Unauthorized"
// @Failure  403             {object}  httpx.ErrorResponse  "Insufficient permissions"
// @Failure  422             {object}  httpx.ErrorResponse  "Validation failed"
// @Router   /admin/audit-logs [get]
func (h *Handler) queryAuditLogs(c echo.Context) error {
	var q AuditLogsQuery
	if err := httpx.BindAndValidate(c, &q); err != nil {
		return err
	}

	orgID, err := parseOptionalUUID(q.OrganizationID)
	if err != nil {
		return err
	}
	userID, err := parseOptionalUUID(q.UserID)
	if err != nil {
		return err
	}
	from, err := parseOptionalRFC3339(q.From)
	if err != nil {
		return err
	}
	to, err := parseOptionalRFC3339(q.To)
	if err != nil {
		return err
	}

	resp, err := h.service.QueryAuditLogs(c.Request().Context(), orgID, userID, buildActionPatterns(q.Actions), from, to, q.limit(), q.offset())
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp)
}

// systemStats returns platform-wide counts for the console's landing page.
// @Summary  Get platform-wide system stats
// @Tags     admin
// @Security BearerAuth
// @Produce  json
// @Success  200  {object}  StatsResponse
// @Failure  401  {object}  httpx.ErrorResponse  "Unauthorized"
// @Failure  403  {object}  httpx.ErrorResponse  "Insufficient permissions"
// @Router   /admin/system/stats [get]
func (h *Handler) systemStats(c echo.Context) error {
	resp, err := h.service.SystemStats(c.Request().Context())
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp)
}

// listPlans returns every subscription plan.
// @Summary  List subscription plans
// @Tags     admin
// @Security BearerAuth
// @Produce  json
// @Success  200  {object}  PlansListResponse
// @Failure  401  {object}  httpx.ErrorResponse  "Unauthorized"
// @Failure  403  {object}  httpx.ErrorResponse  "Insufficient permissions"
// @Router   /admin/plans [get]
func (h *Handler) listPlans(c echo.Context) error {
	resp, err := h.service.ListPlans(c.Request().Context())
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp)
}

// orgIDParam parses the :orgId path param. A malformed id can never match
// an organization row, so it resolves to the same 404 a valid-but-unknown
// id would.
func orgIDParam(c echo.Context) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Param("orgId"))
	if err != nil {
		return uuid.Nil, apperror.New(apperror.NotFound)
	}
	return id, nil
}

// userIDParam parses the :userId path param, same reasoning as orgIDParam.
func userIDParam(c echo.Context) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Param("userId"))
	if err != nil {
		return uuid.Nil, apperror.New(apperror.UserNotFound)
	}
	return id, nil
}

// parseOptionalUUID parses an already-validated (`validate:"omitempty,uuid"`)
// optional query filter into a *uuid.UUID, nil for an absent/empty filter.
func parseOptionalUUID(s *string) (*uuid.UUID, error) {
	if s == nil || *s == "" {
		return nil, nil
	}
	id, err := uuid.Parse(*s)
	if err != nil {
		// The "uuid" validator tag should already have caught this; fail
		// safely rather than trust that invariant blindly.
		return nil, echo.NewHTTPError(http.StatusUnprocessableEntity, "Validation failed")
	}
	return &id, nil
}

// parseOptionalRFC3339 parses an optional ?from=/?to= filter, normalizing
// to UTC — audit_logs.created_at has no time zone, so a non-UTC offset
// must be resolved here or the comparison in AdminQueryAuditLogs would
// silently compare against the wrong wall-clock instant. Mirrors
// auditlog.Handler.query's identical handling of ?since=.
func parseOptionalRFC3339(s *string) (*time.Time, error) {
	if s == nil || *s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, *s)
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusUnprocessableEntity, "Validation failed")
	}
	utc := t.UTC()
	return &utc, nil
}
