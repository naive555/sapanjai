package connector

import (
	"time"

	"github.com/google/uuid"
)

// CreateRequest is the POST /connectors body. Config carries the upstream's
// connection secrets (DB credentials, API keys, ...) — it is sealed before
// it touches the database and is never echoed back in any response.
type CreateRequest struct {
	Name   string         `json:"name" validate:"required,min=1,max=100"`
	Type   string         `json:"type" validate:"required,connectortype"`
	Config map[string]any `json:"config" validate:"required,min=1"`
}

// UpdateRequest is the PATCH /connectors/:connectorId body. Every field is
// optional; an absent field leaves the stored value untouched. There is
// deliberately no Type — changing it would orphan a config blob shaped for a
// different upstream.
type UpdateRequest struct {
	Name   *string        `json:"name" validate:"omitempty,min=1,max=100"`
	Status *string        `json:"status" validate:"omitempty,oneof=active inactive error"`
	Config map[string]any `json:"config" validate:"omitempty,min=1"`
}

// ConnectorResponse is the connector row as clients see it.
//
// It has no config field and must never grow one: the decrypted config is
// the customer's upstream credential, and this module's whole job is that it
// only ever travels service → upstream.
type ConnectorResponse struct {
	ID                uuid.UUID  `json:"id"`
	OrganizationID    uuid.UUID  `json:"organizationId"`
	Name              string     `json:"name"`
	Type              string     `json:"type"`
	Status            string     `json:"status"`
	LastHealthCheckAt *time.Time `json:"lastHealthCheckAt"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
}

// SuccessResponse is the response body for delete.
type SuccessResponse struct {
	Success bool `json:"success"`
}
