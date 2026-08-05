package organization

import (
	"time"

	"github.com/google/uuid"
)

// CreateRequest is the POST /organizations body, mirroring
// OrgModel.createBody in the source app.
type CreateRequest struct {
	Name string `json:"name" validate:"required,min=1"`
	Slug string `json:"slug" validate:"required,min=2,orgslug"`
}

// InviteRequest is the POST /organizations/invite body, mirroring
// OrgModel.inviteBody.
type InviteRequest struct {
	Email string `json:"email" validate:"required,email"`
	Role  string `json:"role" validate:"required,oneof=admin member"`
}

// OrgResponse is the raw organization row shape, returned directly by
// POST /organizations and embedded in each element of GET /organizations.
type OrgResponse struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// MembershipResponse is one element of the GET /organizations response: a
// membership row with its organization embedded.
type MembershipResponse struct {
	ID             uuid.UUID   `json:"id"`
	UserID         uuid.UUID   `json:"userId"`
	OrganizationID uuid.UUID   `json:"organizationId"`
	Role           string      `json:"role"`
	CreatedAt      time.Time   `json:"createdAt"`
	Organization   OrgResponse `json:"organization"`
}

// SuccessResponse is the response body for invite and remove-member.
type SuccessResponse struct {
	Success bool `json:"success"`
}

// MemberResponse is one element of the GET /organizations/members response:
// a roster entry combining the user and their role in the active org.
type MemberResponse struct {
	UserID      uuid.UUID `json:"userId"`
	Email       string    `json:"email"`
	DisplayName *string   `json:"displayName"`
	Role        string    `json:"role"`
	JoinedAt    time.Time `json:"joinedAt"`
}
