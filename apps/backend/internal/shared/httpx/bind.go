// Package httpx holds small HTTP-layer helpers shared across module
// handlers.
package httpx

import (
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
