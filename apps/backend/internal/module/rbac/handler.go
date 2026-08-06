package rbac

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/sapanjai/backend/internal/infra/database/db"
	appmw "github.com/sapanjai/backend/internal/middleware"
	"github.com/sapanjai/backend/internal/shared/apperror"
	"github.com/sapanjai/backend/internal/shared/httpx"
)

// Handler implements the four /rbac routes, mirroring
// src/modules/rbac/index.ts.
type Handler struct {
	service *Service
}

// NewHandler builds an rbac Handler.
func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// Register mounts the four /rbac routes on the given group. All four are
// org-scoped per docs/02-api-contract.md.
func (h *Handler) Register(g *echo.Group, guards *appmw.Guards) {
	g.GET("/roles", h.listRoles, guards.RequireOrg())
	g.POST("/roles", h.createRole, guards.RequireOrg())
	g.PUT("/roles/:roleId/permissions", h.updatePermissions, guards.RequireOrg())
	g.POST("/assign", h.assignRole, guards.RequireOrg())
}

// listRoles returns the active organization's custom roles with permissions
// embedded.
// @Summary  List roles
// @Tags     rbac
// @Security BearerAuth
// @Produce  json
// @Param    x-organization-id  header  string  true  "Active organization ID"
// @Success  200                {array}  RoleResponse
// @Failure  400                {object}  httpx.ErrorResponse  "Missing x-organization-id header"
// @Failure  403                {object}  httpx.ErrorResponse  "Not a member of this organization"
// @Router   /rbac/roles [get]
func (h *Handler) listRoles(c echo.Context) error {
	roles, err := h.service.ListRoles(c.Request().Context(), appmw.OrgID(c))
	if err != nil {
		return err
	}

	out := make([]RoleResponse, len(roles))
	for i, r := range roles {
		out[i] = toRoleResponse(r)
	}

	return c.JSON(http.StatusOK, out)
}

// createRole creates a role and sets its initial permission set in one
// transaction.
// @Summary  Create a role
// @Tags     rbac
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    x-organization-id  header    string             true  "Active organization ID"
// @Param    body               body      CreateRoleRequest  true  "Role payload"
// @Success  200                {object}  RoleRowResponse
// @Failure  400                {object}  httpx.ErrorResponse  "Missing x-organization-id header"
// @Failure  403                {object}  httpx.ErrorResponse  "Not a member of this organization"
// @Failure  422                {object}  httpx.ErrorResponse  "Validation failed"
// @Router   /rbac/roles [post]
func (h *Handler) createRole(c echo.Context) error {
	var req CreateRoleRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	role, err := h.service.CreateRole(c.Request().Context(), appmw.OrgID(c), req.Name, req.Description, req.Permissions)
	if err != nil {
		return err
	}

	return c.JSON(http.StatusOK, toRoleRowResponse(role))
}

// updatePermissions replaces a role's permission set.
// @Summary  Replace a role's permissions
// @Tags     rbac
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    x-organization-id  header    string                     true  "Active organization ID"
// @Param    roleId             path      string                     true  "Role ID"
// @Param    body               body      UpdatePermissionsRequest  true  "New permission set"
// @Success  200                {object}  SuccessResponse
// @Failure  400                {object}  httpx.ErrorResponse  "Missing x-organization-id header"
// @Failure  403                {object}  httpx.ErrorResponse  "Not a member of this organization"
// @Failure  404                {object}  httpx.ErrorResponse  "ROLE_NOT_FOUND"
// @Failure  422                {object}  httpx.ErrorResponse  "Validation failed"
// @Router   /rbac/roles/{roleId}/permissions [put]
func (h *Handler) updatePermissions(c echo.Context) error {
	roleID, err := uuid.Parse(c.Param("roleId"))
	if err != nil {
		// A malformed id can never match a role row.
		return apperror.New(apperror.RoleNotFound)
	}

	var req UpdatePermissionsRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	if err := h.service.UpdatePermissions(c.Request().Context(), roleID, appmw.OrgID(c), req.Permissions); err != nil {
		return err
	}

	return c.JSON(http.StatusOK, SuccessResponse{Success: true})
}

// assignRole assigns a custom role to a member's membership.
// @Summary  Assign a role to a member
// @Tags     rbac
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    x-organization-id  header    string             true  "Active organization ID"
// @Param    body               body      AssignRoleRequest  true  "Assignment payload"
// @Success  200                {object}  SuccessResponse
// @Failure  400                {object}  httpx.ErrorResponse  "Missing x-organization-id header"
// @Failure  403                {object}  httpx.ErrorResponse  "Not a member of this organization"
// @Failure  404                {object}  httpx.ErrorResponse  "MEMBER_NOT_FOUND / ROLE_NOT_FOUND"
// @Failure  422                {object}  httpx.ErrorResponse  "Validation failed"
// @Router   /rbac/assign [post]
func (h *Handler) assignRole(c echo.Context) error {
	var req AssignRoleRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	userID, err := uuid.Parse(req.UserID)
	if err != nil {
		return apperror.New(apperror.MemberNotFound)
	}
	roleID, err := uuid.Parse(req.RoleID)
	if err != nil {
		return apperror.New(apperror.RoleNotFound)
	}

	if err := h.service.AssignRole(c.Request().Context(), appmw.OrgID(c), userID, roleID); err != nil {
		return err
	}

	return c.JSON(http.StatusOK, SuccessResponse{Success: true})
}

func toRoleRowResponse(role db.Role) RoleRowResponse {
	return RoleRowResponse{
		ID:             role.ID,
		OrganizationID: role.OrganizationID,
		Name:           role.Name,
		Description:    role.Description,
		CreatedAt:      role.CreatedAt,
	}
}

func toRoleResponse(r RoleWithPermissions) RoleResponse {
	perms := make([]PermissionResponse, len(r.Permissions))
	for i, p := range r.Permissions {
		perms[i] = PermissionResponse{
			ID:        p.ID,
			RoleID:    p.RoleID,
			Action:    p.Action,
			CreatedAt: p.CreatedAt,
		}
	}

	return RoleResponse{
		ID:             r.Role.ID,
		OrganizationID: r.Role.OrganizationID,
		Name:           r.Role.Name,
		Description:    r.Role.Description,
		CreatedAt:      r.Role.CreatedAt,
		Permissions:    perms,
	}
}
