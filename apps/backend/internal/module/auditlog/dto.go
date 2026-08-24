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

	// Actions holds every repeated ?action= value. Echo's DefaultBinder
	// (confirmed against echo v4.15.4's bindData in bind.go) populates a
	// []string field with the full data[key] slice for a query tag, so a
	// single ?action=x still yields a one-element slice — the "single
	// action=x behaves exactly as before" integration test locks this in.
	// A completely absent action param leaves this nil (Go's zero value for
	// a slice), which service.Query/QueryAuditLogsParams must keep passing
	// through as SQL NULL, never an empty slice — an empty text[] would
	// match zero rows via ANY() and silently break the unfiltered listing.
	Actions []string `query:"action"`

	// Since is bound as a raw *string, not *time.Time. Echo's DefaultBinder
	// would otherwise resolve a *time.Time query field through time.Time's
	// encoding.TextUnmarshaler (RFC3339), and a parse failure there
	// surfaces as an *echo.HTTPError raised from inside c.Bind itself —
	// which httpx.BindAndValidate blanket-rewrites to 400 "Invalid request
	// body", not the contract's 422 "Validation failed" for a malformed
	// filter value (verified by reading bind.go's unmarshalInputToField).
	// Binding as a string and parsing it explicitly in the handler (same
	// shape as the existing UserID -> uuid.Parse pattern below) lets the
	// handler return the correct 422 instead.
	Since *string `query:"since"`

	Limit *int `query:"limit" validate:"omitempty,min=1,max=100"`
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
