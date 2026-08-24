package auditlog

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/sapanjai/backend/internal/infra/database/db"
	appmw "github.com/sapanjai/backend/internal/middleware"
	"github.com/sapanjai/backend/internal/shared/httpx"
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
// optionally filtered by userId/action(s)/since and capped by limit
// (1-100, default 50). action may repeat (?action=a&action=b) to match any
// of several actions; a single ?action=x behaves exactly as before
// repeatable filtering was added. since is an RFC3339 timestamp lower
// bound, inclusive of a row created at exactly that instant.
// @Summary  Query audit logs
// @Tags     audit-logs
// @Security BearerAuth
// @Produce  json
// @Param    x-organization-id  header    string    true   "Active organization ID"
// @Param    userId             query     string    false  "Filter by user ID"
// @Param    action             query     []string  false  "Filter by action; repeatable (?action=a&action=b) to match any"  collectionFormat(multi)
// @Param    since              query     string    false  "Only logs at or after this RFC3339 timestamp"
// @Param    limit              query     int       false  "Max results (1-100, default 50)"
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

	var since *time.Time
	if q.Since != nil {
		t, err := time.Parse(time.RFC3339, *q.Since)
		if err != nil {
			return echo.NewHTTPError(http.StatusUnprocessableEntity, "Validation failed")
		}
		// audit_logs.created_at is a `timestamp` column with no time zone,
		// storing naive UTC wall-clock values. An RFC3339 input carrying a
		// non-UTC offset (e.g. +07:00) must be normalized here, or the
		// comparison in QueryAuditLogs would silently compare against the
		// wrong wall-clock instant for any non-UTC client.
		utc := t.UTC()
		since = &utc
	}

	limit := int32(defaultQueryLimit)
	if q.Limit != nil {
		limit = int32(*q.Limit)
	}

	logs, err := h.service.Query(c.Request().Context(), appmw.OrgID(c), userID, q.Actions, since, limit)
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
