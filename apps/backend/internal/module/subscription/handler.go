package subscription

import (
	"encoding/json"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/sapanjai/backend/internal/infra/database/db"
	appmw "github.com/sapanjai/backend/internal/middleware"

	// Imported for its side effect on `make swagger`, not for code: swaggo
	// resolves the httpx.ErrorResponse in the @Failure annotations below
	// through this file's own import list, and fails generation with
	// "cannot find type definition" without it. Every other handler imports
	// httpx for BindAndValidate; this one has no request body left to bind
	// since POST /subscription/assign was removed.
	_ "github.com/sapanjai/backend/internal/shared/httpx"
)

// Handler implements GET /subscription and GET /plans. The source app's
// POST /subscription/assign is deliberately not mirrored — see Register.
type Handler struct {
	service *Service
}

// NewHandler builds a subscription Handler.
func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// Register mounts the /subscription read route on the given group. It is
// org-scoped per docs/02-api-contract.md.
//
// There is deliberately no tenant-facing write route here. POST
// /subscription/assign used to let any org member — including a plain
// `member` — put their own organization on any plan, since RequireOrg is
// membership-only and carries no role or permission check. Changing a plan
// changes what the org is allowed to consume (max_members, max_roles,
// max_connectors, enforced by Service.EnforceLimit), so it is a
// commercial decision, not a tenant setting. It now lives exclusively at
// POST /admin/organizations/:orgId/plan, behind RequirePlatformRole
// ("superadmin"). The route is gone rather than kept-and-denied: the route
// table should not advertise a capability no tenant token can ever use.
func (h *Handler) Register(g *echo.Group, guards *appmw.Guards) {
	g.GET("", h.get, guards.RequireOrg())
}

// RegisterPlans mounts GET /plans. Not in the source app — added in Phase 6
// to populate the frontend's plan picker, which no longer exists (see
// Register above). It stays as a read-only catalogue: the tenant
// subscription page shows which plan the org is on against the others it
// could be moved to, and a plan's name and limits are not secret. Plans are
// global, not org-scoped, so this only requires RequireAuth, not RequireOrg.
// See docs/03 "Deviations resolved during Phase 6".
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
