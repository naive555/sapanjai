package connector

import "sort"

// Type identifies a connector's upstream system. The column is plain text
// with no database-level enum, so extending the set is one constant here and
// no migration — see .claude/plans/plan.md decision 1.
type Type string

// TypeGeneric is the placeholder the skeleton ships with. Real types
// (flowaccount, peak, ...) land with their adapters; see docs/05-mcp-gateway.md
// Phase 2.
const TypeGeneric Type = "generic"

var validTypes = map[Type]struct{}{
	TypeGeneric: {},
}

// Status is a connector's last known health.
type Status string

const (
	StatusActive   Status = "active"
	StatusInactive Status = "inactive"
	StatusError    Status = "error"
)

var validStatuses = map[Status]struct{}{
	StatusActive:   {},
	StatusInactive: {},
	StatusError:    {},
}

// RBAC actions gating the connector routes. Owners bypass these entirely
// (rbac.Service.HasPermission); any other member needs a role granting the
// exact action, "connector:*", or "*".
const (
	PermissionRead   = "connector:read"
	PermissionWrite  = "connector:write"
	PermissionDelete = "connector:delete"
)

// IsValidType reports whether s names a known connector type. Used by the
// "connectortype" request validator and re-checked in the service.
func IsValidType(s string) bool {
	_, ok := validTypes[Type(s)]
	return ok
}

// IsValidStatus reports whether s names a known status.
func IsValidStatus(s string) bool {
	_, ok := validStatuses[Status(s)]
	return ok
}

// AllTypes returns every known type, sorted, for docs and error text.
func AllTypes() []string {
	out := make([]string, 0, len(validTypes))
	for t := range validTypes {
		out = append(out, string(t))
	}
	sort.Strings(out)
	return out
}
