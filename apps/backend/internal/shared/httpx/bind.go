// Package httpx holds small HTTP-layer helpers shared across module
// handlers.
package httpx

import (
	"encoding/json"
	"net/http"

	"github.com/labstack/echo/v4"
)

// BindAndValidate binds the request body into req and validates it,
// producing the exact contract errors from docs/02-api-contract.md:
// malformed JSON -> 400 "Invalid request body"; a body that parses but
// fails struct validation -> 422 "Validation failed". Handlers should
// return the result directly.
func BindAndValidate(c echo.Context, req any) error {
	if err := c.Bind(req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "Invalid request body")
	}
	if err := c.Validate(req); err != nil {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "Validation failed")
	}
	return nil
}

// BindBodyAndValidate behaves like BindAndValidate, except it decodes the
// request body directly and binds nothing else: no path parameters, no query
// parameters, no content-type negotiation, and no dependence on the HTTP
// method.
//
// Its caller is DELETE /admin/organizations/:orgId, whose body carries the
// confirmation slug and re-auth password (docs/11-admin-panel.md D4). It is
// easy to assume this helper is what makes that body readable at all; it is
// not. As of echo v4.15.4, DefaultBinder.Bind's GET/DELETE/HEAD special case
// gates only BindQueryParams and BindBody runs for every method, so plain
// BindAndValidate would read that body too. What this buys is independence
// from that rule, which echo has moved before (its own source cites issue
// #1670 and a pre-v4.1.11 behavior it restored). On the one
// route where a silently-empty confirmation field turns a destructive-action
// guard into a no-op, not tracking a third-party binder's method semantics is
// worth eight lines. TestBindAndValidate's "bind valid delete" case pins the
// current behavior so a change surfaces there first.
func BindBodyAndValidate(c echo.Context, req any) error {
	if err := json.NewDecoder(c.Request().Body).Decode(req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "Invalid request body")
	}
	if err := c.Validate(req); err != nil {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "Validation failed")
	}
	return nil
}
