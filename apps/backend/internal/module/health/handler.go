// Package health exposes the public liveness endpoint.
package health

import (
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
)

// Handler serves GET /health, mirroring src/modules/health in the source
// app: { status: "ok", uptime: <seconds since process start> }.
type Handler struct {
	startedAt time.Time
}

func NewHandler() *Handler {
	return &Handler{startedAt: time.Now()}
}

// Register mounts the health route on the given group/router.
func (h *Handler) Register(e *echo.Echo) {
	e.GET("/health", h.check)
}

// check reports liveness.
// @Summary  Health check
// @Tags     health
// @Produce  json
// @Success  200  {object}  map[string]any  "{ status: \"ok\", uptime: <seconds> }"
// @Router   /health [get]
func (h *Handler) check(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]any{
		"status": "ok",
		"uptime": time.Since(h.startedAt).Seconds(),
	})
}
