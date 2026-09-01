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

// Handler implements the /admin read routes (execution plan Phase 2) and
// the superadmin-only mutation routes (Phase 3). Reads are guarded by
// RequirePlatformRole("superadmin", "support") — docs/11-admin-panel.md
// §4's role matrix: support reads everything. Mutations are guarded by a
// separate RequirePlatformRole("superadmin") instance — support mutates
// nothing, so a compromised support account cannot destroy anything.
// Impersonation (Phase 4) sits on the read guard — see Register.
type Handler struct {
	service *Service
}

// NewHandler builds an admin Handler.
func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// Register mounts every Phase 2 read route and every Phase 3 mutation route
// under g. Two separate RequirePlatformRole instances back the two groups —
// not one shared guard with a per-handler role check — so the route table
// itself is the source of truth for which routes support may reach; see
// docs/11-admin-panel.md §4.
func (h *Handler) Register(g *echo.Group, guards *appmw.Guards) {
	read := guards.RequirePlatformRole("superadmin", "support")
	write := guards.RequirePlatformRole("superadmin")

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

	g.POST("/organizations/:orgId/plan", h.assignPlan, write)
	g.PUT("/organizations/:orgId/limits", h.setOrgLimits, write)
	g.DELETE("/organizations/:orgId", h.deleteOrganization, write)
	g.PATCH("/users/:userId/platform-role", h.changePlatformRole, write)
	g.PATCH("/users/:userId/ban", h.setBan, write)
	g.POST("/plans", h.createPlan, write)
	g.PUT("/plans/:planId", h.updatePlan, write)
	g.DELETE("/plans/:planId", h.deletePlan, write)

	// Impersonation is on the READ guard, not write: it grants only what
	// support already has (docs/11-admin-panel.md §4's matrix lists it for
	// both roles), and the token it mints is itself read-only. It is a POST
	// purely because it has a body and a side effect worth auditing.
	g.POST("/users/:userId/impersonate", h.impersonate, read)

	// TOTP step-up (Phase 6, Task 6.3) sits on RequirePlatformRoleNo2FA, NOT
	// on `read`/`write` above (both of which enforce ADMIN_REQUIRE_2FA) —
	// the chicken-and-egg the guard's own doc comment names: a staff member
	// cannot complete step-up through a route step-up itself gates. Both
	// roles get all three: enroll/confirm set up 2FA, verify is the step-up
	// check every other route depends on.
	noTwoFA := guards.RequirePlatformRoleNo2FA("superadmin", "support")
	g.POST("/2fa/enroll", h.enrollTOTP, noTwoFA)
	g.POST("/2fa/confirm", h.confirmTOTP, noTwoFA)
	g.POST("/2fa/verify", h.verifyTOTP, noTwoFA)
}

// adminContext builds the AdminContext every Phase 3 mutation passes to its
// service method: the calling admin's own id (from RequirePlatformRole via
// appmw.UserID) plus the ip/userAgent captured here in the handler, never
// inside the service (execution plan Task 3.1 — CLAUDE.md's
// handler->service->sqlc convention: no HTTP concerns inside a service).
//
// c.RealIP() is governed by e.IPExtractor (server.go, Phase 6 Task 6.1) and
// by apps/frontend's proxy stripping any inbound X-Forwarded-For/X-Real-IP
// before forwarding — a caller can no longer inject an arbitrary chain, so
// this is no longer attacker-controlled input. It is still not a
// per-staff-member location, though: every admin request is relayed through
// that same frontend proxy, so this resolves to the frontend's own private
// network address for all of them. See AdminContext's doc comment and
// server.go's e.IPExtractor comment for the full trust chain.
func adminContext(c echo.Context) AdminContext {
	return AdminContext{
		AdminID:   appmw.UserID(c),
		IP:        c.RealIP(),
		UserAgent: c.Request().UserAgent(),
	}
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

// ==== Mutations (execution plan Phase 3) ====

// assignPlan assigns an organization to a subscription plan.
// @Summary  Assign an organization's subscription plan
// @Tags     admin
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    orgId  path      string             true  "Organization ID"
// @Param    body   body      AssignPlanRequest  true  "Plan payload"
// @Success  200    {object}  SuccessResponse
// @Failure  401    {object}  httpx.ErrorResponse  "Unauthorized"
// @Failure  403    {object}  httpx.ErrorResponse  "Insufficient permissions"
// @Failure  422    {object}  httpx.ErrorResponse  "Validation failed"
// @Router   /admin/organizations/{orgId}/plan [post]
func (h *Handler) assignPlan(c echo.Context) error {
	orgID, err := orgIDParam(c)
	if err != nil {
		return err
	}
	var req AssignPlanRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	planID, err := uuid.Parse(req.PlanID)
	if err != nil {
		return err
	}

	if err := h.service.AssignPlan(c.Request().Context(), adminContext(c), orgID, planID); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, SuccessResponse{Success: true})
}

// setOrgLimits overwrites an organization's custom subscription limits.
// @Summary  Set an organization's custom subscription limits
// @Tags     admin
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    orgId  path      string             true  "Organization ID"
// @Param    body   body      SetLimitsRequest   true  "customLimits payload; null clears back to plan-only limits"
// @Success  200    {object}  SuccessResponse
// @Failure  401    {object}  httpx.ErrorResponse  "Unauthorized"
// @Failure  403    {object}  httpx.ErrorResponse  "Insufficient permissions"
// @Failure  404    {object}  httpx.ErrorResponse  "Organization has no subscription to set limits on"
// @Failure  422    {object}  httpx.ErrorResponse  "Validation failed"
// @Router   /admin/organizations/{orgId}/limits [put]
func (h *Handler) setOrgLimits(c echo.Context) error {
	orgID, err := orgIDParam(c)
	if err != nil {
		return err
	}
	var req SetLimitsRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	// nil clears the override entirely and has nothing to validate; a
	// present object must hold whole numbers, same as a plan's own limits.
	if req.CustomLimits != nil {
		if err := validateCustomLimits(*req.CustomLimits); err != nil {
			return echo.NewHTTPError(http.StatusUnprocessableEntity, "Validation failed")
		}
	}

	if err := h.service.SetOrgLimits(c.Request().Context(), adminContext(c), orgID, req.CustomLimits); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, SuccessResponse{Success: true})
}

// deleteOrganization deletes an organization after re-authentication and a
// slug confirmation. DELETE bodies are not bound by Echo's default binder
// (see httpx.BindBodyAndValidate's doc comment) — this is the one route in
// this package that needs it.
// @Summary  Delete an organization
// @Tags     admin
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    orgId  path      string                      true  "Organization ID"
// @Param    body   body      DeleteOrganizationRequest  true  "confirm must equal the organization's slug; password re-verifies the caller"
// @Success  200    {object}  SuccessResponse
// @Failure  401    {object}  httpx.ErrorResponse  "Unauthorized"
// @Failure  403    {object}  httpx.ErrorResponse  "REAUTH_FAILED / Insufficient permissions"
// @Failure  404    {object}  httpx.ErrorResponse  "Resource not found"
// @Failure  400    {object}  httpx.ErrorResponse  "ORG_CONFIRM_MISMATCH"
// @Failure  429    {object}  httpx.ErrorResponse  "TOO_MANY_ATTEMPTS"
// @Router   /admin/organizations/{orgId} [delete]
func (h *Handler) deleteOrganization(c echo.Context) error {
	orgID, err := orgIDParam(c)
	if err != nil {
		return err
	}
	var req DeleteOrganizationRequest
	if err := httpx.BindBodyAndValidate(c, &req); err != nil {
		return err
	}

	if err := h.service.DeleteOrganization(c.Request().Context(), adminContext(c), orgID, req.Confirm, req.Password); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, SuccessResponse{Success: true})
}

// changePlatformRole grants or revokes a user's platform_role.
// @Summary  Grant or revoke a user's platform role
// @Tags     admin
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    userId  path      string                true  "User ID"
// @Param    body    body      PlatformRoleRequest  true  "role is null to revoke; password re-verifies the caller"
// @Success  200     {object}  SuccessResponse
// @Failure  401     {object}  httpx.ErrorResponse  "Unauthorized"
// @Failure  403     {object}  httpx.ErrorResponse  "REAUTH_FAILED / CANNOT_TARGET_SELF / Insufficient permissions"
// @Failure  404     {object}  httpx.ErrorResponse  "USER_NOT_FOUND"
// @Failure  409     {object}  httpx.ErrorResponse  "SUPERADMIN_LIMIT"
// @Failure  422     {object}  httpx.ErrorResponse  "Validation failed"
// @Failure  429     {object}  httpx.ErrorResponse  "TOO_MANY_ATTEMPTS"
// @Router   /admin/users/{userId}/platform-role [patch]
func (h *Handler) changePlatformRole(c echo.Context) error {
	userID, err := userIDParam(c)
	if err != nil {
		return err
	}
	var req PlatformRoleRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	if err := h.service.ChangePlatformRole(c.Request().Context(), adminContext(c), userID, req.Role, req.Password); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, SuccessResponse{Success: true})
}

// setBan bans or unbans a user.
// @Summary  Ban or unban a user
// @Tags     admin
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    userId  path      string      true  "User ID"
// @Param    body    body      BanRequest  true  "banned toggles the ban; reason is stored on ban; password re-verifies the caller"
// @Success  200     {object}  SuccessResponse
// @Failure  401     {object}  httpx.ErrorResponse  "Unauthorized"
// @Failure  403     {object}  httpx.ErrorResponse  "REAUTH_FAILED / CANNOT_TARGET_SELF / Insufficient permissions"
// @Failure  404     {object}  httpx.ErrorResponse  "USER_NOT_FOUND"
// @Failure  409     {object}  httpx.ErrorResponse  "TARGET_IS_PLATFORM_STAFF"
// @Failure  422     {object}  httpx.ErrorResponse  "Validation failed"
// @Failure  429     {object}  httpx.ErrorResponse  "TOO_MANY_ATTEMPTS"
// @Router   /admin/users/{userId}/ban [patch]
func (h *Handler) setBan(c echo.Context) error {
	userID, err := userIDParam(c)
	if err != nil {
		return err
	}
	var req BanRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	if err := h.service.SetBan(c.Request().Context(), adminContext(c), userID, req.Banned, req.Reason, req.Password); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, SuccessResponse{Success: true})
}

// createPlan creates a new subscription plan.
// @Summary  Create a subscription plan
// @Tags     admin
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    body  body      PlanCreateRequest  true  "Plan payload"
// @Success  200   {object}  PlanItem
// @Failure  401   {object}  httpx.ErrorResponse  "Unauthorized"
// @Failure  403   {object}  httpx.ErrorResponse  "Insufficient permissions"
// @Failure  422   {object}  httpx.ErrorResponse  "Validation failed"
// @Router   /admin/plans [post]
func (h *Handler) createPlan(c echo.Context) error {
	var req PlanCreateRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	if err := validatePlanLimits(req.Limits); err != nil {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "Validation failed")
	}

	item, err := h.service.CreatePlan(c.Request().Context(), adminContext(c), req.Name, req.Limits)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, item)
}

// updatePlan replaces a subscription plan's name and limits.
// @Summary  Update a subscription plan
// @Tags     admin
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    planId  path      string             true  "Plan ID"
// @Param    body    body      PlanUpdateRequest  true  "Plan payload"
// @Success  200     {object}  PlanItem
// @Failure  401     {object}  httpx.ErrorResponse  "Unauthorized"
// @Failure  403     {object}  httpx.ErrorResponse  "Insufficient permissions"
// @Failure  404     {object}  httpx.ErrorResponse  "Resource not found"
// @Failure  422     {object}  httpx.ErrorResponse  "Validation failed"
// @Router   /admin/plans/{planId} [put]
func (h *Handler) updatePlan(c echo.Context) error {
	planID, err := planIDParam(c)
	if err != nil {
		return err
	}
	var req PlanUpdateRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	if err := validatePlanLimits(req.Limits); err != nil {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "Validation failed")
	}

	item, err := h.service.UpdatePlan(c.Request().Context(), adminContext(c), planID, req.Name, req.Limits)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, item)
}

// deletePlan deletes a subscription plan, refusing one with active
// subscriptions.
// @Summary  Delete a subscription plan
// @Tags     admin
// @Security BearerAuth
// @Produce  json
// @Param    planId  path      string  true  "Plan ID"
// @Success  200     {object}  SuccessResponse
// @Failure  401     {object}  httpx.ErrorResponse  "Unauthorized"
// @Failure  403     {object}  httpx.ErrorResponse  "Insufficient permissions"
// @Failure  404     {object}  httpx.ErrorResponse  "Resource not found"
// @Failure  409     {object}  httpx.ErrorResponse  "PLAN_IN_USE"
// @Router   /admin/plans/{planId} [delete]
func (h *Handler) deletePlan(c echo.Context) error {
	planID, err := planIDParam(c)
	if err != nil {
		return err
	}

	if err := h.service.DeletePlan(c.Request().Context(), adminContext(c), planID); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, SuccessResponse{Success: true})
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

// planIDParam parses the :planId path param, same reasoning as orgIDParam.
func planIDParam(c echo.Context) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Param("planId"))
	if err != nil {
		return uuid.Nil, apperror.New(apperror.NotFound)
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

// impersonate mints a short-lived read-only token authenticating as the
// target user.
// @Summary  Start impersonating a tenant user (read-only, 10 minutes)
// @Tags     admin
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    userId  path      string              true  "User ID to impersonate"
// @Param    body    body      ImpersonateRequest  true  "Mandatory reason, minimum 10 characters"
// @Success  200     {object}  ImpersonateResponse
// @Failure  401     {object}  httpx.ErrorResponse  "Unauthorized"
// @Failure  403     {object}  httpx.ErrorResponse  "Insufficient permissions, or the target is platform staff / suspended"
// @Failure  404     {object}  httpx.ErrorResponse  "User not found"
// @Failure  422     {object}  httpx.ErrorResponse  "Validation failed"
// @Router   /admin/users/{userId}/impersonate [post]
func (h *Handler) impersonate(c echo.Context) error {
	targetID, err := userIDParam(c)
	if err != nil {
		return err
	}
	var req ImpersonateRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	resp, err := h.service.Impersonate(c.Request().Context(), adminContext(c), targetID, req.Reason)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp)
}

// ==== TOTP step-up (execution plan Phase 6, Task 6.3) ====

// enrollTOTP generates a fresh secret for the calling staff member and
// returns its otpauth:// URI once. Sits on RequirePlatformRoleNo2FA — the
// chicken-and-egg is that a staff member with ADMIN_REQUIRE_2FA=true cannot
// reach a 2FA-gated route to set 2FA up in the first place.
// @Summary  Enroll a fresh TOTP secret (returns the otpauth:// URI once)
// @Tags     admin
// @Security BearerAuth
// @Produce  json
// @Success  200  {object}  TOTPEnrollResponse
// @Failure  401  {object}  httpx.ErrorResponse  "Unauthorized"
// @Failure  403  {object}  httpx.ErrorResponse  "Insufficient permissions"
// @Router   /admin/2fa/enroll [post]
func (h *Handler) enrollTOTP(c echo.Context) error {
	resp, err := h.service.EnrollTOTP(c.Request().Context(), appmw.UserID(c), appmw.UserEmail(c))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp)
}

// confirmTOTP verifies the first code from a just-enrolled authenticator
// and returns ten recovery codes once.
// @Summary  Confirm TOTP enrollment (returns ten recovery codes once)
// @Tags     admin
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    body  body      TOTPConfirmRequest  true  "6-digit code from the authenticator app"
// @Success  200   {object}  TOTPConfirmResponse
// @Failure  400   {object}  httpx.ErrorResponse  "TOTP_NOT_ENROLLED"
// @Failure  401   {object}  httpx.ErrorResponse  "INVALID_TOTP_CODE / Unauthorized"
// @Failure  403   {object}  httpx.ErrorResponse  "Insufficient permissions"
// @Failure  422   {object}  httpx.ErrorResponse  "Validation failed"
// @Router   /admin/2fa/confirm [post]
func (h *Handler) confirmTOTP(c echo.Context) error {
	var req TOTPConfirmRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	resp, err := h.service.ConfirmTOTP(c.Request().Context(), appmw.UserID(c), req.Code)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp)
}

// verifyTOTP is the step-up check: on success it sets the admin:2fa:<userId>
// Redis key every other /admin route requires when ADMIN_REQUIRE_2FA=true.
// code is tried as a live TOTP code first, then as an unused recovery code.
// @Summary  Verify a TOTP or recovery code (step-up, 12h)
// @Tags     admin
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    body  body      TOTPVerifyRequest  true  "TOTP code or an unused recovery code"
// @Success  200   {object}  SuccessResponse
// @Failure  400   {object}  httpx.ErrorResponse  "TOTP_NOT_ENROLLED"
// @Failure  401   {object}  httpx.ErrorResponse  "INVALID_TOTP_CODE / Unauthorized"
// @Failure  403   {object}  httpx.ErrorResponse  "Insufficient permissions"
// @Failure  422   {object}  httpx.ErrorResponse  "Validation failed"
// @Failure  429   {object}  httpx.ErrorResponse  "TOO_MANY_ATTEMPTS"
// @Router   /admin/2fa/verify [post]
func (h *Handler) verifyTOTP(c echo.Context) error {
	var req TOTPVerifyRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	if err := h.service.VerifyTOTP(c.Request().Context(), appmw.UserID(c), req.Code); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, SuccessResponse{Success: true})
}
