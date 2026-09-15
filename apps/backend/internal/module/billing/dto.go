package billing

// CheckoutRequest is the POST /billing/checkout body.
//
// PlanID is a string carrying a UUID rather than a uuid.UUID, following
// rbac.AssignRequest and admin's AssignPlanRequest: `required,uuid` on a
// string gives a clean 422 "Validation failed" for a malformed id, where
// `required` on a uuid.UUID would have to reason about a zero [16]byte.
//
// Interval is optional and defaults to "month" (Service.Checkout). It selects
// which plan_prices row to charge; currency is not a parameter at all, since
// plan decision 3 is THB only. There is deliberately NO field for a currency,
// a coupon, a trial, a quantity, or a price id: every one of those is a way
// for a tenant to influence what it is charged, and the price a plan sells
// for is server-side catalogue data.
type CheckoutRequest struct {
	PlanID   string `json:"planId" validate:"required,uuid"`
	Interval string `json:"interval" validate:"omitempty,oneof=month year"`
}

// RedirectResponse is the body of both /billing routes: the hosted Stripe
// URL the browser should be sent to.
//
// One shape for both because the frontend does the same thing with either —
// window.location.assign(url). Hosted Checkout and the hosted Portal are
// redirects, not embedded flows, which is why apps/frontend loads no
// Stripe.js and needs no CSP work; keep it that way.
type RedirectResponse struct {
	URL string `json:"url"`
}

// WebhookResponse is POST /billing/webhook's 200 body.
//
// Stripe only reads the status code, so the body exists for humans: a
// `curl` or a Stripe CLI `trigger` against a misconfigured deployment gets
// something legible back instead of an empty 200 that could equally have
// come from a proxy. There is deliberately nothing in it about what the
// event did — an unauthenticated caller who guesses a body should learn
// nothing, and a caller holding the signing secret can read the audit log.
type WebhookResponse struct {
	Received bool `json:"received"`
}
