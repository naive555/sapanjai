package auditlog

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/junctera/backend/internal/infra/database/db"
	appmw "github.com/junctera/backend/internal/middleware"
	"github.com/junctera/backend/internal/shared/httpx"
)

const defaultQueryLimit = 50

// Handler implements the GET /audit-logs route, mirroring
// src/modules/audit-log/index.ts.
type Handler struct {
	service *Service
}

// NewHandler builds an auditlog Handler.
func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// Register mounts GET /audit-logs on the given group. Org-scoped per
// docs/02-api-contract.md.
func (h *Handler) Register(g *echo.Group, guards *appmw.Guards) {
	g.GET("", h.query, guards.RequireOrg())
}

// query returns the active organization's audit logs, newest first,
// optionally filtered by userId/action and capped by limit (1-100, default 50).
// @Summary  Query audit logs
// @Tags     audit-logs
// @Security BearerAuth
// @Produce  json
// @Param    x-organization-id  header    string  true   "Active organization ID"
// @Param    userId             query     string  false  "Filter by user ID"
// @Param    action             query     string  false  "Filter by action"
// @Param    limit              query     int     false  "Max results (1-100, default 50)"
// @Success  200                {array}   LogResponse
// @Failure  400                {object}  httpx.ErrorResponse  "Missing x-organization-id header"
// @Failure  403                {object}  httpx.ErrorResponse  "Not a member of this organization"
// @Failure  422                {object}  httpx.ErrorResponse  "Validation failed"
// @Router   /audit-logs [get]
func (h *Handler) query(c echo.Context) error {
	var q QueryParams
	if err := httpx.BindAndValidate(c, &q); err != nil {
		return err
	}

	var userID *uuid.UUID
	if q.UserID != nil {
		id, err := uuid.Parse(*q.UserID)
		if err != nil {
			return err
		}
		userID = &id
	}

	limit := int32(defaultQueryLimit)
	if q.Limit != nil {
		limit = int32(*q.Limit)
	}

	logs, err := h.service.Query(c.Request().Context(), appmw.OrgID(c), userID, q.Action, limit)
	if err != nil {
		return err
	}

	out := make([]LogResponse, len(logs))
	for i, l := range logs {
		out[i] = toLogResponse(l)
	}

	return c.JSON(http.StatusOK, out)
}

func toLogResponse(l db.AuditLog) LogResponse {
	return LogResponse{
		ID:             l.ID,
		OrganizationID: fromPgUUID(l.OrganizationID),
		UserID:         fromPgUUID(l.UserID),
		Action:         l.Action,
		Metadata:       nonEmptyJSON(l.Metadata),
		CreatedAt:      l.CreatedAt,
	}
}
