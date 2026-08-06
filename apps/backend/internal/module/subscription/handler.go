package subscription

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/sapanjai/backend/internal/infra/database/db"
	appmw "github.com/sapanjai/backend/internal/middleware"
	"github.com/sapanjai/backend/internal/shared/httpx"
)

// Handler implements the two /subscription routes, mirroring
// src/modules/subscription/index.ts.
type Handler struct {
	service *Service
}

// NewHandler builds a subscription Handler.
func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// Register mounts the two /subscription routes on the given group. Both are
// org-scoped per docs/02-api-contract.md.
func (h *Handler) Register(g *echo.Group, guards *appmw.Guards) {
	g.GET("", h.get, guards.RequireOrg())
	g.POST("/assign", h.assign, guards.RequireOrg())
}

// RegisterPlans mounts GET /plans. Not in the source app — added in Phase 6
// so the frontend subscription page can populate a plan picker (plan ids are
// server-generated UUIDs with no fixed/knowable value, so the frontend has
// no other way to discover them). Plans are global, not org-scoped, so this
// only requires RequireAuth, not RequireOrg. See docs/03 "Deviations
// resolved during Phase 6".
func (h *Handler) RegisterPlans(g *echo.Group, guards *appmw.Guards) {
	g.GET("", h.listPlans, guards.RequireAuth())
}

// get returns the active organization's subscription with its plan embedded,
// or null if the organization has no subscription.
// @Summary  Get the organization's subscription
// @Tags     subscription
// @Security BearerAuth
// @Produce  json
// @Param    x-organization-id  header    string  true  "Active organization ID"
// @Success  200                {object}  SubscriptionResponse
// @Failure  400                {object}  httpx.ErrorResponse  "Missing x-organization-id header"
// @Failure  403                {object}  httpx.ErrorResponse  "Not a member of this organization"
// @Router   /subscription [get]
func (h *Handler) get(c echo.Context) error {
	sub, err := h.service.GetSubscription(c.Request().Context(), appmw.OrgID(c))
	if err != nil {
		return err
	}
	if sub == nil {
		return c.JSON(http.StatusOK, nil)
	}

	return c.JSON(http.StatusOK, toSubscriptionResponse(*sub))
}

// assign upserts the active organization's subscription to the given plan.
// @Summary  Assign a subscription plan
// @Tags     subscription
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    x-organization-id  header    string         true  "Active organization ID"
// @Param    body               body      AssignRequest  true  "Plan payload"
// @Success  200                {object}  SuccessResponse
// @Failure  400                {object}  httpx.ErrorResponse  "Missing x-organization-id header"
// @Failure  403                {object}  httpx.ErrorResponse  "Not a member of this organization"
// @Failure  422                {object}  httpx.ErrorResponse  "Validation failed"
// @Router   /subscription/assign [post]
func (h *Handler) assign(c echo.Context) error {
	var req AssignRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	planID, err := uuid.Parse(req.PlanID)
	if err != nil {
		return err
	}

	if err := h.service.AssignPlan(c.Request().Context(), appmw.OrgID(c), planID); err != nil {
		return err
	}

	return c.JSON(http.StatusOK, SuccessResponse{Success: true})
}

// listPlans returns every available subscription plan.
// @Summary  List subscription plans
// @Tags     subscription
// @Security BearerAuth
// @Produce  json
// @Success  200  {array}  PlanResponse
// @Failure  401  {object}  httpx.ErrorResponse  "Unauthorized"
// @Router   /plans [get]
func (h *Handler) listPlans(c echo.Context) error {
	plans, err := h.service.ListPlans(c.Request().Context())
	if err != nil {
		return err
	}

	out := make([]PlanResponse, len(plans))
	for i, p := range plans {
		out[i] = PlanResponse{ID: p.ID, Name: p.Name, Limits: p.Limits, CreatedAt: p.CreatedAt}
	}

	return c.JSON(http.StatusOK, out)
}

func toSubscriptionResponse(row db.GetOrgSubscriptionRow) SubscriptionResponse {
	var customLimits json.RawMessage
	if len(row.CustomLimits) > 0 {
		customLimits = row.CustomLimits
	}

	return SubscriptionResponse{
		ID:             row.ID,
		OrganizationID: row.OrganizationID,
		PlanID:         row.PlanID,
		CustomLimits:   customLimits,
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
		Plan: PlanResponse{
			ID:        row.PlanPid,
			Name:      row.PlanName,
			Limits:    row.PlanPlimits,
			CreatedAt: row.PlanCreatedAt,
		},
	}
}
