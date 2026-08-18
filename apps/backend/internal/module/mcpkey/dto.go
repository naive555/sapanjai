package mcpkey

import (
	"time"

	"github.com/google/uuid"
)

// CreateRequest is the POST /mcp-keys body. expiresInDays is optional; a
// nil value mints a key that never expires.
type CreateRequest struct {
	Name          string `json:"name" validate:"required,min=1,max=100"`
	ExpiresInDays *int   `json:"expiresInDays" validate:"omitempty,min=1"`
}

// CreateResponse is the POST /mcp-keys body. ApiKey carries the raw token
// and is populated **only** here — no other response in this module ever
// includes it, and it is not persisted anywhere after this response is
// written.
type CreateResponse struct {
	ID        uuid.UUID  `json:"id"`
	Name      string     `json:"name"`
	APIKey    string     `json:"apiKey"`
	ExpiresAt *time.Time `json:"expiresAt"`
	CreatedAt time.Time  `json:"createdAt"`
}

// MCPKeyResponse is one row as GET /mcp-keys returns it. It never carries
// KeyHash or the raw token — only metadata a dashboard needs to let an
// owner recognize and revoke a key.
type MCPKeyResponse struct {
	ID             uuid.UUID  `json:"id"`
	OrganizationID uuid.UUID  `json:"organizationId"`
	UserID         uuid.UUID  `json:"userId"`
	Name           string     `json:"name"`
	Scopes         []string   `json:"scopes"`
	LastUsedAt     *time.Time `json:"lastUsedAt"`
	ExpiresAt      *time.Time `json:"expiresAt"`
	RevokedAt      *time.Time `json:"revokedAt"`
	CreatedAt      time.Time  `json:"createdAt"`
}

// SuccessResponse is the response body for revoke.
type SuccessResponse struct {
	Success bool `json:"success"`
}
