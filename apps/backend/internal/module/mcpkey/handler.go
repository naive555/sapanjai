package mcpkey

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

// RBAC actions gating the /mcp-keys routes. Owners bypass these entirely
// (rbac.Service.HasPermission); any other member needs a role granting the
// exact action, "mcpkey:*", or "*".
const (
	PermissionRead   = "mcpkey:read"
	PermissionWrite  = "mcpkey:write"
	PermissionDelete = "mcpkey:delete"
)

// Handler implements the three /mcp-keys routes.
type Handler struct {
	service *Service
}

// NewHandler builds an mcpkey Handler.
func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// Register mounts the three /mcp-keys routes, mirroring the connector
// module's RequirePermission pattern (internal/module/connector).
func (h *Handler) Register(g *echo.Group, guards *appmw.Guards) {
	g.POST("", h.create, guards.RequirePermission(PermissionWrite))
	g.GET("", h.list, guards.RequirePermission(PermissionRead))
	g.DELETE("/:keyId", h.revoke, guards.RequirePermission(PermissionDelete))
}

// create mints a new MCP key for the caller's active organization. The raw
// token is returned in this response and nowhere else — it is not stored
// anywhere and cannot be recovered later.
// @Summary  Create an MCP API key
// @Tags     mcp-keys
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    x-organization-id  header    string         true  "Active organization ID"
// @Param    body               body      CreateRequest  true  "MCP key payload"
// @Success  200                {object}  CreateResponse
// @Failure  400                {object}  httpx.ErrorResponse  "Missing x-organization-id header"
// @Failure  403                {object}  httpx.ErrorResponse  "Missing permission: mcpkey:write"
// @Failure  409                {object}  httpx.ErrorResponse  "MCP_KEY_NAME_TAKEN"
// @Failure  422                {object}  httpx.ErrorResponse  "Validation failed"
// @Router   /mcp-keys [post]
func (h *Handler) create(c echo.Context) error {
	var req CreateRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	row, rawToken, err := h.service.Create(c.Request().Context(), appmw.OrgID(c), appmw.UserID(c), req.Name, req.ExpiresInDays, req.Scopes)
	if err != nil {
		return err
	}

	return c.JSON(http.StatusOK, CreateResponse{
		ID:        row.ID,
		Name:      row.Name,
		APIKey:    rawToken,
		ExpiresAt: fromPgTimestamp(row.ExpiresAt),
		CreatedAt: row.CreatedAt,
	})
}

// list returns the active organization's MCP keys, oldest first. Never
// includes key_hash or a raw token.
// @Summary  List MCP API keys
// @Tags     mcp-keys
// @Security BearerAuth
// @Produce  json
// @Param    x-organization-id  header    string  true  "Active organization ID"
// @Success  200                {array}   MCPKeyResponse
// @Failure  400                {object}  httpx.ErrorResponse  "Missing x-organization-id header"
// @Failure  403                {object}  httpx.ErrorResponse  "Missing permission: mcpkey:read"
// @Router   /mcp-keys [get]
func (h *Handler) list(c echo.Context) error {
	rows, err := h.service.List(c.Request().Context(), appmw.OrgID(c))
	if err != nil {
		return err
	}

	out := make([]MCPKeyResponse, len(rows))
	for i, row := range rows {
		out[i] = toMCPKeyResponse(row)
	}

	return c.JSON(http.StatusOK, out)
}

// revoke sets revoked_at on a key scoped to the active organization. A key
// belonging to another org resolves to the same 404 as a nonexistent one.
// @Summary  Revoke an MCP API key
// @Tags     mcp-keys
// @Security BearerAuth
// @Produce  json
// @Param    x-organization-id  header    string  true  "Active organization ID"
// @Param    keyId              path      string  true  "MCP key ID"
// @Success  200                {object}  SuccessResponse
// @Failure  400                {object}  httpx.ErrorResponse  "Missing x-organization-id header"
// @Failure  403                {object}  httpx.ErrorResponse  "Missing permission: mcpkey:delete"
// @Failure  404                {object}  httpx.ErrorResponse  "MCP_KEY_NOT_FOUND"
// @Router   /mcp-keys/{keyId} [delete]
func (h *Handler) revoke(c echo.Context) error {
	id, err := keyIDParam(c)
	if err != nil {
		return err
	}

	if err := h.service.Revoke(c.Request().Context(), appmw.OrgID(c), id); err != nil {
		return err
	}

	return c.JSON(http.StatusOK, SuccessResponse{Success: true})
}

// keyIDParam parses the :keyId path param.
func keyIDParam(c echo.Context) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Param("keyId"))
	if err != nil {
		// A malformed id can never match a key row.
		return uuid.Nil, apperror.New(apperror.MCPKeyNotFound)
	}
	return id, nil
}

func toMCPKeyResponse(row db.McpApiKey) MCPKeyResponse {
	return MCPKeyResponse{
		ID:             row.ID,
		OrganizationID: row.OrganizationID,
		UserID:         row.UserID,
		Name:           row.Name,
		Scopes:         row.Scopes,
		LastUsedAt:     fromPgTimestamp(row.LastUsedAt),
		ExpiresAt:      fromPgTimestamp(row.ExpiresAt),
		RevokedAt:      fromPgTimestamp(row.RevokedAt),
		CreatedAt:      row.CreatedAt,
	}
	// Intentionally does not map KeyHash. Do not add it.
}

// fromPgTimestamp converts a nullable pg_catalog.timestamp column to a Go
// pointer, mirroring connector's fromPgTimestamp for the same nullable-
// column shape sqlc emits.
func fromPgTimestamp(t pgtype.Timestamp) *time.Time {
	if !t.Valid {
		return nil
	}
	tm := t.Time
	return &tm
}
