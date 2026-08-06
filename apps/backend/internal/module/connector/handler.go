package connector

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"github.com/sapanjai/backend/internal/infra/database/db"
	appmw "github.com/sapanjai/backend/internal/middleware"
	"github.com/sapanjai/backend/internal/shared/apperror"
	"github.com/sapanjai/backend/internal/shared/httpx"
)

// Handler implements the six /connectors routes.
type Handler struct {
	service *Service
}

// NewHandler builds a connector Handler.
func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// Register mounts the six /connectors routes. Every route is gated by an
// RBAC permission rather than plain membership — this is the first
// production use of RequirePermission (see docs/02-api-contract.md).
func (h *Handler) Register(g *echo.Group, guards *appmw.Guards) {
	g.POST("", h.create, guards.RequirePermission(PermissionWrite))
	g.GET("", h.list, guards.RequirePermission(PermissionRead))
	g.GET("/:connectorId", h.get, guards.RequirePermission(PermissionRead))
	g.PATCH("/:connectorId", h.update, guards.RequirePermission(PermissionWrite))
	g.DELETE("/:connectorId", h.delete, guards.RequirePermission(PermissionDelete))
	// Health check writes status and last_health_check_at, so it needs
	// write, not read.
	g.POST("/:connectorId/health-check", h.healthCheck, guards.RequirePermission(PermissionWrite))
}

// create seals the request's config and inserts a new connector, scoped to
// the caller's active organization.
// @Summary  Create a connector
// @Tags     connectors
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    x-organization-id  header    string         true  "Active organization ID"
// @Param    body               body      CreateRequest  true  "Connector payload"
// @Success  200                {object}  ConnectorResponse
// @Failure  400                {object}  httpx.ErrorResponse  "Missing x-organization-id header"
// @Failure  403                {object}  httpx.ErrorResponse  "Missing permission: connector:write / LIMIT_EXCEEDED"
// @Failure  409                {object}  httpx.ErrorResponse  "CONNECTOR_NAME_TAKEN"
// @Failure  422                {object}  httpx.ErrorResponse  "Validation failed / INVALID_CONNECTOR_TYPE"
// @Router   /connectors [post]
func (h *Handler) create(c echo.Context) error {
	var req CreateRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	connector, err := h.service.Create(c.Request().Context(), appmw.OrgID(c), appmw.UserID(c), req.Name, req.Type, req.Config)
	if err != nil {
		return err
	}

	return c.JSON(http.StatusOK, toConnectorResponse(connector))
}

// list returns the active organization's connectors, oldest first. Never
// includes config.
// @Summary  List connectors
// @Tags     connectors
// @Security BearerAuth
// @Produce  json
// @Param    x-organization-id  header    string  true  "Active organization ID"
// @Success  200                {array}   ConnectorResponse
// @Failure  400                {object}  httpx.ErrorResponse  "Missing x-organization-id header"
// @Failure  403                {object}  httpx.ErrorResponse  "Missing permission: connector:read"
// @Router   /connectors [get]
func (h *Handler) list(c echo.Context) error {
	rows, err := h.service.List(c.Request().Context(), appmw.OrgID(c))
	if err != nil {
		return err
	}

	out := make([]ConnectorResponse, len(rows))
	for i, row := range rows {
		out[i] = toConnectorResponse(row)
	}

	return c.JSON(http.StatusOK, out)
}

// get returns one connector scoped to the active organization. A connector
// belonging to another org resolves to the same 404 as a nonexistent one.
// @Summary  Get a connector
// @Tags     connectors
// @Security BearerAuth
// @Produce  json
// @Param    x-organization-id  header    string  true  "Active organization ID"
// @Param    connectorId        path      string  true  "Connector ID"
// @Success  200                {object}  ConnectorResponse
// @Failure  400                {object}  httpx.ErrorResponse  "Missing x-organization-id header"
// @Failure  403                {object}  httpx.ErrorResponse  "Missing permission: connector:read"
// @Failure  404                {object}  httpx.ErrorResponse  "NOT_FOUND"
// @Router   /connectors/{connectorId} [get]
func (h *Handler) get(c echo.Context) error {
	id, err := connectorIDParam(c)
	if err != nil {
		return err
	}

	connector, err := h.service.Get(c.Request().Context(), appmw.OrgID(c), id)
	if err != nil {
		return err
	}

	return c.JSON(http.StatusOK, toConnectorResponse(connector))
}

// update applies a partial patch to a connector. type is immutable and has
// no field in UpdateRequest; a supplied config is re-sealed under a fresh
// data key.
// @Summary  Update a connector
// @Tags     connectors
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    x-organization-id  header    string         true  "Active organization ID"
// @Param    connectorId        path      string         true  "Connector ID"
// @Param    body               body      UpdateRequest  true  "Connector patch"
// @Success  200                {object}  ConnectorResponse
// @Failure  400                {object}  httpx.ErrorResponse  "Missing x-organization-id header"
// @Failure  403                {object}  httpx.ErrorResponse  "Missing permission: connector:write"
// @Failure  404                {object}  httpx.ErrorResponse  "NOT_FOUND"
// @Failure  422                {object}  httpx.ErrorResponse  "Validation failed"
// @Router   /connectors/{connectorId} [patch]
func (h *Handler) update(c echo.Context) error {
	id, err := connectorIDParam(c)
	if err != nil {
		return err
	}

	var req UpdateRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	connector, err := h.service.Update(c.Request().Context(), appmw.OrgID(c), appmw.UserID(c), id, req.Name, req.Status, req.Config)
	if err != nil {
		return err
	}

	return c.JSON(http.StatusOK, toConnectorResponse(connector))
}

// delete removes a connector from the active organization.
// @Summary  Delete a connector
// @Tags     connectors
// @Security BearerAuth
// @Produce  json
// @Param    x-organization-id  header    string  true  "Active organization ID"
// @Param    connectorId        path      string  true  "Connector ID"
// @Success  200                {object}  SuccessResponse
// @Failure  400                {object}  httpx.ErrorResponse  "Missing x-organization-id header"
// @Failure  403                {object}  httpx.ErrorResponse  "Missing permission: connector:delete"
// @Failure  404                {object}  httpx.ErrorResponse  "NOT_FOUND"
// @Router   /connectors/{connectorId} [delete]
func (h *Handler) delete(c echo.Context) error {
	id, err := connectorIDParam(c)
	if err != nil {
		return err
	}

	if err := h.service.Delete(c.Request().Context(), appmw.OrgID(c), appmw.UserID(c), id); err != nil {
		return err
	}

	return c.JSON(http.StatusOK, SuccessResponse{Success: true})
}

// healthCheck probes a connector's upstream and records the outcome. With no
// Checker registered for the connector's type (the skeleton registers none),
// this returns 501 HEALTH_CHECK_UNSUPPORTED and leaves the row untouched.
// @Summary  Check a connector's health
// @Tags     connectors
// @Security BearerAuth
// @Produce  json
// @Param    x-organization-id  header    string  true  "Active organization ID"
// @Param    connectorId        path      string  true  "Connector ID"
// @Success  200                {object}  ConnectorResponse
// @Failure  400                {object}  httpx.ErrorResponse  "Missing x-organization-id header"
// @Failure  403                {object}  httpx.ErrorResponse  "Missing permission: connector:write"
// @Failure  404                {object}  httpx.ErrorResponse  "NOT_FOUND"
// @Failure  501                {object}  httpx.ErrorResponse  "HEALTH_CHECK_UNSUPPORTED"
// @Router   /connectors/{connectorId}/health-check [post]
func (h *Handler) healthCheck(c echo.Context) error {
	id, err := connectorIDParam(c)
	if err != nil {
		return err
	}

	connector, err := h.service.CheckHealth(c.Request().Context(), appmw.OrgID(c), id)
	if err != nil {
		return err
	}

	return c.JSON(http.StatusOK, toConnectorResponse(connector))
}

// connectorIDParam parses the :connectorId path param.
func connectorIDParam(c echo.Context) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Param("connectorId"))
	if err != nil {
		// A malformed id can never match a connector row.
		return uuid.Nil, apperror.New(apperror.NotFound)
	}
	return id, nil
}

func toConnectorResponse(row db.Connector) ConnectorResponse {
	return ConnectorResponse{
		ID:                row.ID,
		OrganizationID:    row.OrganizationID,
		Name:              row.Name,
		Type:              row.Type,
		Status:            row.Status,
		LastHealthCheckAt: fromPgTimestamp(row.LastHealthCheckAt),
		CreatedAt:         row.CreatedAt,
		UpdatedAt:         row.UpdatedAt,
	}
	// Intentionally does not map EncryptedConfig. Do not add it.
}

// fromPgTimestamp converts a nullable pg_catalog.timestamp column to a Go
// pointer, mirroring auditlog's fromPgUUID for the same nullable-column
// shape sqlc emits.
func fromPgTimestamp(t pgtype.Timestamp) *time.Time {
	if !t.Valid {
		return nil
	}
	tm := t.Time
	return &tm
}
