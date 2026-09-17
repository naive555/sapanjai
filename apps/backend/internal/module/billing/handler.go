package billing

import (
	"io"
	"net/http"

	"github.com/labstack/echo/v4"

	appmw "github.com/sapanjai/backend/internal/middleware"
	"github.com/sapanjai/backend/internal/shared/httpx"
)

// PermissionWrite gates the two money-changing /billing routes: checkout
// and portal.
//
// Not RequireOrg, which is membership-only. That distinction is the entire
// reason POST /subscription/assign was deleted from the template: sitting on
// RequireOrg, it let any `member` move their own org onto any plan and lift
// max_members/max_roles/max_connectors on themselves. A route that changes
// what an org pays for is at least as sensitive, so both routes here take an
// RBAC action (plan invariant 4 / decision 2).
//
// One action for both, not a read/write split between them. The Portal can
// cancel the subscription, change the payment method, and expose invoices
// carrying a billing address — it is if anything the more dangerous of the
// two, so it never gets a weaker guard than Checkout.
//
// The RBAC engine's owner bypass (rbac.Service.HasPermission) means an owner
// needs no special case, and a customer can delegate billing to a finance
// person by granting this one action without making them an org owner.
const PermissionWrite = "billing:write"

// PermissionRead gates GET /billing/usage — the read view PermissionWrite's
// doc comment used to describe as "a later step" (billing plan step 9,
// this one). Deliberately narrower than PermissionWrite: reading how many
// calls an org has made this month cannot cancel a subscription or move
// money, so it does not need checkout/portal's guard — a finance person
// granted only billing:read can watch the meter without also being able to
// change what the org pays for. The invoice view the original comment also
// promised is not part of this step; see the plan for what remains.
const PermissionRead = "billing:read"

// Handler implements the three RBAC-guarded /billing routes (checkout,
// portal, usage) plus the separately-mounted webhook (RegisterWebhook,
// below).
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
// mounts separately, via RegisterWebhook.
func (h *Handler) Register(g *echo.Group, guards *appmw.Guards) {
	g.POST("/checkout", h.checkout, guards.RequirePermission(PermissionWrite))
	g.POST("/portal", h.portal, guards.RequirePermission(PermissionWrite))
	g.GET("/usage", h.usage, guards.RequirePermission(PermissionRead))
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

// usage returns the caller's active organization's tool-call usage for the
// current UTC calendar month: the live count the gateway's own quota check
// enforces against, the resolved monthly cap, and a per-tool breakdown.
// See Service.Usage for how each field is resolved.
//
// organizationID is appmw.OrgID(c), the same guard-verified header every
// other /billing and /subscription route reads it from — there is no input
// through which a caller can read another org's usage.
// @Summary  Get the organization's current tool-call usage
// @Tags     billing
// @Security BearerAuth
// @Produce  json
// @Param    x-organization-id  header    string  true  "Active organization ID"
// @Success  200                {object}  UsageResponse
// @Failure  400                {object}  httpx.ErrorResponse  "Missing x-organization-id header"
// @Failure  403                {object}  httpx.ErrorResponse  "Missing permission: billing:read"
// @Router   /billing/usage [get]
func (h *Handler) usage(c echo.Context) error {
	resp, err := h.service.Usage(c.Request().Context(), appmw.OrgID(c))
	if err != nil {
		return err
	}

	return c.JSON(http.StatusOK, resp)
}

// WebhookPath is POST /billing/webhook's absolute path. It is mounted
// directly on the Echo instance by RegisterWebhook, NOT on the group
// Register uses, and the constant is exported so server.go and the
// integration tests name the same string.
const WebhookPath = "/billing/webhook"

// maxWebhookBodyBytes caps the body this route will read. Stripe event
// payloads are well under this; the cap exists because the route is
// unauthenticated by necessity, so an unbounded io.ReadAll on it is a free
// memory-exhaustion primitive for anyone who finds the URL. Rejection is a
// 400, the same as any other body that cannot be verified.
const maxWebhookBodyBytes = 1 << 20 // 1 MiB

// RegisterWebhook mounts POST /billing/webhook.
//
// Mounted on the Echo instance rather than on a group, and with no auth
// guard of any kind, because Stripe presents no JWT and no
// x-organization-id — a webhook behind RequireAuth/RequireOrg/
// RequirePermission could never be reached by Stripe at all. Its
// authentication is the Stripe-Signature HMAC, checked in
// Service.HandleWebhook before anything is written.
//
// Deliberately NOT e.Group("/billing", ...): Echo's Group registers
// catch-all RouteNotFound entries for its prefix as soon as it carries
// middleware, so a second group on "/billing" would quietly wrap the
// already-mounted guarded routes' 404 behaviour in this route's middleware.
// One explicit route has no such side effect.
//
// middleware is the caller's (server.go) IP allowlist, applied per-route.
func (h *Handler) RegisterWebhook(e *echo.Echo, middleware ...echo.MiddlewareFunc) {
	e.POST(WebhookPath, h.webhook, middleware...)
}

// webhook verifies and reconciles one Stripe event.
//
// # The raw body
//
// Signature verification is an HMAC over the EXACT bytes Stripe sent, so
// this reads and retains them before anything else can touch the reader,
// and never calls c.Bind (or httpx.BindAndValidate, or anything else that
// would consume c.Request().Body). In Echo the failure is concrete and
// quiet: a consumed body leaves an empty reader, the HMAC is computed over
// nothing, and verification fails — or, worse, passes in a test that
// helpfully re-supplies the body.
//
// This also depends on the global middleware stack staying body-blind.
// Recover, RequestID, and requestLogger (server.go) all are. Nothing that
// reads a request body may be added globally.
// @Summary  Stripe webhook receiver
// @Tags     billing
// @Accept   json
// @Produce  json
// @Param    Stripe-Signature  header    string  true  "Stripe webhook signature"
// @Success  200               {object}  WebhookResponse
// @Failure  400               {object}  httpx.ErrorResponse  "WEBHOOK_SIGNATURE_INVALID / Invalid request body"
// @Failure  404               {object}  httpx.ErrorResponse  "Route not found (caller outside STRIPE_WEBHOOK_IP_ALLOWLIST)"
// @Failure  501               {object}  httpx.ErrorResponse  "BILLING_NOT_CONFIGURED"
// @Router   /billing/webhook [post]
func (h *Handler) webhook(c echo.Context) error {
	req := c.Request()

	payload, err := io.ReadAll(http.MaxBytesReader(c.Response(), req.Body, maxWebhookBodyBytes))
	if err != nil {
		// Oversized or truncated. Nothing to verify, so it stops here;
		// the error is not echoed back, it could quote the body's size
		// and shape to an anonymous caller for no benefit.
		return echo.NewHTTPError(http.StatusBadRequest, "Invalid request body")
	}

	if err := h.service.HandleWebhook(req.Context(), payload, req.Header.Get("Stripe-Signature")); err != nil {
		return err
	}

	return c.JSON(http.StatusOK, WebhookResponse{Received: true})
}
