// Package middleware holds cross-cutting Echo middleware shared across
// modules.
package middleware

import (
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

const RequestIDHeader = "X-Request-Id"

// RequestID reads X-Request-Id from the incoming request, or generates a
// fresh UUID if absent, and echoes the value back on the response header —
// mirroring src/hooks/request-id.ts in the source app.
func RequestID() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			id := c.Request().Header.Get(RequestIDHeader)
			if id == "" {
				id = uuid.NewString()
			}

			c.Set("requestId", id)
			c.Response().Header().Set(RequestIDHeader, id)

			return next(c)
		}
	}
}
