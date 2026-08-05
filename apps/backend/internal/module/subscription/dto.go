package subscription

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// AssignRequest is the POST /subscription/assign body, mirroring the
// t.Object({ planId: t.String() }) schema in the source app.
type AssignRequest struct {
	PlanID string `json:"planId" validate:"required,uuid"`
}

// PlanResponse is the plan embedded in SubscriptionResponse.
type PlanResponse struct {
	ID        uuid.UUID       `json:"id"`
	Name      string          `json:"name"`
	Limits    json.RawMessage `json:"limits"`
	CreatedAt time.Time       `json:"createdAt"`
}

// SubscriptionResponse is the GET /subscription body: the org's
// subscription with its plan embedded.
type SubscriptionResponse struct {
	ID             uuid.UUID       `json:"id"`
	OrganizationID uuid.UUID       `json:"organizationId"`
	PlanID         uuid.UUID       `json:"planId"`
	CustomLimits   json.RawMessage `json:"customLimits"`
	CreatedAt      time.Time       `json:"createdAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
	Plan           PlanResponse    `json:"plan"`
}

// SuccessResponse is the response body for POST /subscription/assign.
type SuccessResponse struct {
	Success bool `json:"success"`
}
