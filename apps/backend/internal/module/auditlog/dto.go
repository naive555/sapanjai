package auditlog

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// QueryParams is the GET /audit-logs query string, mirroring
// AuditLogModel.queryParams in the source app.
type QueryParams struct {
	UserID *string `query:"userId" validate:"omitempty,uuid"`
	Action *string `query:"action"`
	Limit  *int    `query:"limit" validate:"omitempty,min=1,max=100"`
}

// LogResponse is one element of the GET /audit-logs response.
type LogResponse struct {
	ID             uuid.UUID       `json:"id"`
	OrganizationID *uuid.UUID      `json:"organizationId"`
	UserID         *uuid.UUID      `json:"userId"`
	Action         string          `json:"action"`
	Metadata       json.RawMessage `json:"metadata"`
	CreatedAt      time.Time       `json:"createdAt"`
}
