package rbac

import (
	"time"

	"github.com/google/uuid"
)

// CreateRoleRequest is the POST /rbac/roles body, mirroring
// RBACModel.createRoleBody in the source app.
type CreateRoleRequest struct {
	Name        string   `json:"name" validate:"required,min=1"`
	Description *string  `json:"description"`
	Permissions []string `json:"permissions" validate:"omitempty,dive,min=1"`
}

// UpdatePermissionsRequest is the PUT /rbac/roles/:roleId/permissions body,
// mirroring RBACModel.updatePermissionsBody.
type UpdatePermissionsRequest struct {
	Permissions []string `json:"permissions" validate:"omitempty,dive,min=1"`
}

// AssignRoleRequest is the POST /rbac/assign body, mirroring
// RBACModel.assignRoleBody.
type AssignRoleRequest struct {
	UserID string `json:"userId" validate:"required,uuid"`
	RoleID string `json:"roleId" validate:"required,uuid"`
}

// RoleRowResponse is the raw role row returned by POST /rbac/roles — no
// permissions key, since the source's createRole returns before
// setPermissions runs.
type RoleRowResponse struct {
	ID             uuid.UUID `json:"id"`
	OrganizationID uuid.UUID `json:"organizationId"`
	Name           string    `json:"name"`
	Description    *string   `json:"description"`
	CreatedAt      time.Time `json:"createdAt"`
}

// RoleResponse is one element of GET /rbac/roles — a role row with its
// permission set embedded.
type RoleResponse struct {
	ID             uuid.UUID            `json:"id"`
	OrganizationID uuid.UUID            `json:"organizationId"`
	Name           string               `json:"name"`
	Description    *string              `json:"description"`
	CreatedAt      time.Time            `json:"createdAt"`
	Permissions    []PermissionResponse `json:"permissions"`
}

// PermissionResponse is one permission row embedded in a RoleResponse.
type PermissionResponse struct {
	ID        uuid.UUID `json:"id"`
	RoleID    uuid.UUID `json:"roleId"`
	Action    string    `json:"action"`
	CreatedAt time.Time `json:"createdAt"`
}

// SuccessResponse is the response body for update-permissions and assign.
type SuccessResponse struct {
	Success bool `json:"success"`
}
