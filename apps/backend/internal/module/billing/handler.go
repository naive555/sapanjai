package billing

import (
	"net/http"

	"github.com/labstack/echo/v4"

	appmw "github.com/sapanjai/backend/internal/middleware"
	"github.com/sapanjai/backend/internal/shared/httpx"
)

// PermissionWrite gates both /billing routes.
//
// Not RequireOrg, which is membership-only. That distinction is the entire
// reason POST /subscription/assign was deleted from the template: sitting on
// RequireOrg, it let any `member` move their own org onto any plan and lift
// max_members/max_roles/max_connectors on themselves. A route that changes
// what an org pays for is at least as sensitive, so both routes here take an
// RBAC action (plan invariant 4 / decision 2).
//
// One action for both, not a read/write split. The Portal can cancel the
// subscription, change the payment method, and expose invoices carrying a
// billing address — it is if anything the more dangerous of the two, so it
// never gets the weaker guard. There is no billing:read action here because
// neither route reads anything; the usage and invoice views that will carry
// billing:read are a later step.
//
// The RBAC engine's owner bypass (rbac.Service.HasPermission) means an owner
// needs no special case, and a customer can delegate billing to a finance
// person by granting this one action without making them an org owner.
const PermissionWrite = "billing:write"

// Handler implements the two /billing routes.
type Handler struct {
	service *Service
}

// NewHandler builds a billing Handler.
func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// Register mounts the /billing routes, following the connector and mcpkey
// modules' RequirePermission pattern.
//
// POST /billing/webhook is deliberately absent: Stripe presents no JWT and
// no x-organization-id, so it cannot live on this guarded group at all. It
// is step 7 of the billing plan and mounts separately.
func (h *Handler) Register(g *echo.Group, guards *appmw.Guards) {
	g.POST("/checkout", h.checkout, guards.RequirePermission(PermissionWrite))
	g.POST("/portal", h.portal, guards.RequirePermission(PermissionWrite))
}

// checkout starts a Stripe Checkout Session for the caller's active
// organization and returns the hosted URL to redirect to.
//
// The organization is appmw.OrgID(c) — resolved by RequirePermission from
// the x-organization-id header after a membership lookup — never a body
// field. There is no way to name another tenant's organization here.
// @Summary  Start a subscription checkout
// @Tags     billing
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    x-organization-id  header    string           true  "Active organization ID"
// @Param    body               body      CheckoutRequest  true  "Checkout payload"
// @Success  200                {object}  RedirectResponse
// @Failure  400                {object}  httpx.ErrorResponse  "Missing x-organization-id header / Invalid request body"
// @Failure  403                {object}  httpx.ErrorResponse  "Missing permission: billing:write"
// @Failure  404                {object}  httpx.ErrorResponse  "NOT_FOUND (unknown or non-public plan)"
// @Failure  409                {object}  httpx.ErrorResponse  "PLAN_NOT_PURCHASABLE"
// @Failure  422                {object}  httpx.ErrorResponse  "Validation failed"
// @Failure  501                {object}  httpx.ErrorResponse  "BILLING_NOT_CONFIGURED"
// @Failure  502                {object}  httpx.ErrorResponse  "BILLING_PROVIDER_ERROR"
// @Router   /billing/checkout [post]
func (h *Handler) checkout(c echo.Context) error {
	var req CheckoutRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	url, err := h.service.Checkout(c.Request().Context(),
		appmw.OrgID(c), appmw.UserID(c), appmw.UserEmail(c), req.PlanID, req.Interval)
	if err != nil {
		return err
	}

	return c.JSON(http.StatusOK, RedirectResponse{URL: url})
}

// portal opens a Stripe Customer Portal session for the caller's active
// organization, which is where upgrade, downgrade, cancellation,
// payment-method changes, and invoice history all happen — none of it built
// here.
// @Summary  Open the billing portal
// @Tags     billing
// @Security BearerAuth
// @Produce  json
// @Param    x-organization-id  header    string  true  "Active organization ID"
// @Success  200                {object}  RedirectResponse
// @Failure  400                {object}  httpx.ErrorResponse  "Missing x-organization-id header"
// @Failure  403                {object}  httpx.ErrorResponse  "Missing permission: billing:write"
// @Failure  501                {object}  httpx.ErrorResponse  "BILLING_NOT_CONFIGURED"
// @Failure  502                {object}  httpx.ErrorResponse  "BILLING_PROVIDER_ERROR"
// @Router   /billing/portal [post]
func (h *Handler) portal(c echo.Context) error {
	url, err := h.service.Portal(c.Request().Context(),
		appmw.OrgID(c), appmw.UserID(c), appmw.UserEmail(c))
	if err != nil {
		return err
	}

	return c.JSON(http.StatusOK, RedirectResponse{URL: url})
}
