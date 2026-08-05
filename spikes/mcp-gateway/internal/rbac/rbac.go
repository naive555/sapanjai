// Package rbac is a dependency-free port of controlplane's permission engine
// (apps/backend/internal/module/rbac/service.go, Service.HasPermission) so the
// spike can exercise the *exact* matching semantics the real gateway will use
// without dragging in postgres.
//
// Semantics preserved verbatim from the source:
//   - a caller with no membership in the org is denied;
//   - membership role "owner" bypasses the role/permission tables entirely;
//   - otherwise the caller's aggregated permission actions are scanned for
//     "*" (superuser), an exact match on "<resource>:<verb>", or the
//     "<resource>:*" wildcard.
//
// The one thing this package does NOT reproduce is where the actions come
// from: the real engine runs ListPermissionActionsByUserOrg against postgres.
// Here they are handed in. That is the whole integration seam — see
// docs/RBAC-MAPPING.md.
package rbac

import "strings"

// Principal is everything the gateway knows about the caller of an MCP
// session: which user, which organization, and what that pairing may do.
// It is the MCP-side equivalent of what RequireOrg puts on an echo.Context
// (auth.userID + auth.orgID + auth.membership).
type Principal struct {
	UserID string
	Email  string
	OrgID  string
	// Role is the membership row's role column ("owner", "admin", "member").
	Role string
	// Actions is the flattened permission set from the caller's assigned
	// roles, e.g. {"invoice:read", "report:*"}. Ignored when Role == "owner".
	Actions []string
}

// HasPermission reports whether p may perform action. Mirrors
// rbac.Service.HasPermission's match order exactly.
func (p *Principal) HasPermission(action string) bool {
	if p == nil || p.OrgID == "" {
		// No membership resolved -> deny. Matches the pgx.ErrNoRows branch
		// in the source, which returns (false, nil) rather than an error.
		return false
	}
	if p.Role == "owner" {
		return true
	}

	resource, _, _ := strings.Cut(action, ":")
	wildcard := resource + ":*"

	for _, a := range p.Actions {
		if a == "*" || a == action || a == wildcard {
			return true
		}
	}
	return false
}
