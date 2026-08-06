package organization

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/sapanjai/backend/internal/infra/database/db"
	appmw "github.com/sapanjai/backend/internal/middleware"
	"github.com/sapanjai/backend/internal/shared/apperror"
	"github.com/sapanjai/backend/internal/shared/httpx"
)

// Handler implements the four /organizations routes, mirroring
// src/modules/organization/index.ts.
type Handler struct {
	service *Service
}

// NewHandler builds an organization Handler.
func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// Register mounts the five /organizations routes on the given group, guarded
// per docs/02-api-contract.md: create/list are auth-only, members/invite/
// remove are org-scoped.
func (h *Handler) Register(g *echo.Group, guards *appmw.Guards) {
	g.POST("", h.create, guards.RequireAuth())
	g.GET("", h.list, guards.RequireAuth())
	g.GET("/members", h.listMembers, guards.RequireOrg())
	g.POST("/invite", h.invite, guards.RequireOrg())
	g.DELETE("/members/:userId", h.removeMember, guards.RequireOrg())
}

// create creates a new organization and an owner membership for the caller.
// @Summary  Create an organization
// @Tags     organizations
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    body  body      CreateRequest  true  "Organization payload"
// @Success  200   {object}  OrgResponse
// @Failure  409   {object}  httpx.ErrorResponse  "SLUG_TAKEN"
// @Failure  422   {object}  httpx.ErrorResponse  "Validation failed"
// @Router   /organizations [post]
func (h *Handler) create(c echo.Context) error {
	var req CreateRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	org, err := h.service.Create(c.Request().Context(), appmw.UserID(c), req.Name, req.Slug)
	if err != nil {
		return err
	}

	return c.JSON(http.StatusOK, toOrgResponse(org))
}

// list returns the caller's memberships with each organization embedded.
// @Summary  List my organizations
// @Tags     organizations
// @Security BearerAuth
// @Produce  json
// @Success  200  {array}  MembershipResponse
// @Router   /organizations [get]
func (h *Handler) list(c echo.Context) error {
	rows, err := h.service.ListByUser(c.Request().Context(), appmw.UserID(c))
	if err != nil {
		return err
	}

	out := make([]MembershipResponse, len(rows))
	for i, row := range rows {
		out[i] = toMembershipResponse(row)
	}

	return c.JSON(http.StatusOK, out)
}

// listMembers returns the active organization's member roster.
// @Summary  List organization members
// @Tags     organizations
// @Security BearerAuth
// @Produce  json
// @Param    x-organization-id  header    string  true  "Active organization ID"
// @Success  200                {array}   MemberResponse
// @Failure  400                {object}  httpx.ErrorResponse  "Missing x-organization-id header"
// @Failure  403                {object}  httpx.ErrorResponse  "Not a member of this organization"
// @Router   /organizations/members [get]
func (h *Handler) listMembers(c echo.Context) error {
	rows, err := h.service.ListMembers(c.Request().Context(), appmw.OrgID(c))
	if err != nil {
		return err
	}

	out := make([]MemberResponse, len(rows))
	for i, row := range rows {
		out[i] = toMemberResponse(row)
	}

	return c.JSON(http.StatusOK, out)
}

// invite adds a new member to the active organization, enforcing the caller's
// role (not "member") and the plan's max_members limit.
// @Summary  Invite a member
// @Tags     organizations
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    x-organization-id  header    string         true  "Active organization ID"
// @Param    body               body      InviteRequest  true  "Invite payload"
// @Success  200                {object}  SuccessResponse
// @Failure  400                {object}  httpx.ErrorResponse  "Missing x-organization-id header"
// @Failure  403                {object}  httpx.ErrorResponse  "FORBIDDEN / LIMIT_EXCEEDED / Not a member of this organization"
// @Failure  404                {object}  httpx.ErrorResponse  "USER_NOT_FOUND"
// @Failure  409                {object}  httpx.ErrorResponse  "ALREADY_MEMBER"
// @Failure  422                {object}  httpx.ErrorResponse  "Validation failed"
// @Router   /organizations/invite [post]
func (h *Handler) invite(c echo.Context) error {
	var req InviteRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	membership := appmw.MembershipFromContext(c)
	err := h.service.Invite(c.Request().Context(), appmw.OrgID(c), appmw.UserID(c), membership.Role, req.Email, req.Role)
	if err != nil {
		return err
	}

	return c.JSON(http.StatusOK, SuccessResponse{Success: true})
}

// removeMember removes a member from the active organization. The caller's
// role must not be "member"; the owner cannot be removed.
// @Summary  Remove a member
// @Tags     organizations
// @Security BearerAuth
// @Produce  json
// @Param    x-organization-id  header    string  true  "Active organization ID"
// @Param    userId             path      string  true  "Member user ID"
// @Success  200                {object}  SuccessResponse
// @Failure  400                {object}  httpx.ErrorResponse  "Missing x-organization-id header"
// @Failure  403                {object}  httpx.ErrorResponse  "CANNOT_REMOVE_OWNER / FORBIDDEN / Not a member of this organization"
// @Failure  404                {object}  httpx.ErrorResponse  "MEMBER_NOT_FOUND"
// @Router   /organizations/members/{userId} [delete]
func (h *Handler) removeMember(c echo.Context) error {
	targetID, err := uuid.Parse(c.Param("userId"))
	if err != nil {
		// A malformed id can never match a member row.
		return apperror.New(apperror.MemberNotFound)
	}

	membership := appmw.MembershipFromContext(c)
	if err := h.service.RemoveMember(c.Request().Context(), appmw.OrgID(c), membership.Role, targetID); err != nil {
		return err
	}

	return c.JSON(http.StatusOK, SuccessResponse{Success: true})
}

func toOrgResponse(org db.Organization) OrgResponse {
	return OrgResponse{
		ID:        org.ID,
		Name:      org.Name,
		Slug:      org.Slug,
		CreatedAt: org.CreatedAt,
		UpdatedAt: org.UpdatedAt,
	}
}

func toMemberResponse(row db.ListOrganizationMembersRow) MemberResponse {
	return MemberResponse{
		UserID:      row.UserID,
		Email:       row.Email,
		DisplayName: row.DisplayName,
		Role:        row.Role,
		JoinedAt:    row.JoinedAt,
	}
}

func toMembershipResponse(row db.ListMembershipsByUserRow) MembershipResponse {
	return MembershipResponse{
		ID:             row.ID,
		UserID:         row.UserID,
		OrganizationID: row.OrganizationID,
		Role:           row.Role,
		CreatedAt:      row.CreatedAt,
		Organization: OrgResponse{
			ID:        row.OrgID,
			Name:      row.OrgName,
			Slug:      row.OrgSlug,
			CreatedAt: row.OrgCreatedAt,
			UpdatedAt: row.OrgUpdatedAt,
		},
	}
}
