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

// BindBodyAndValidate behaves exactly like BindAndValidate, except it
// always parses the request body regardless of HTTP method. Echo's default
// binder (DefaultBinder.Bind) skips BindBody entirely for GET/DELETE/HEAD —
// reasonable for the vast majority of routes, but wrong for
// DELETE /admin/organizations/:orgId, whose body carries the confirmation
// slug and re-auth password (docs/11-admin-panel.md D4). Use this instead
// of BindAndValidate for any route that needs a body on one of those three
// methods.
func BindBodyAndValidate(c echo.Context, req any) error {
	if err := json.NewDecoder(c.Request().Body).Decode(req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "Invalid request body")
	}
	if err := c.Validate(req); err != nil {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "Validation failed")
	}
	return nil
}
