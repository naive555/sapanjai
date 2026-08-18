package rbac

import (
	"strings"

	"github.com/google/uuid"
)

// ActionMatches reports whether granted permits action, under the same
// semantics HasPermission has always used: "*" grants everything, an exact
// match grants that one action, and a "<resource>:*" wildcard grants every
// verb on that resource. resource is derived with strings.Cut(action, ":"),
// so "connector:read" wildcards to "connector:*" and an action with no ":"
// (resource == the whole string) wildcards to "<action>:*", matching the
// original inline behavior byte for byte. Extracted out of HasPermission so
// a bulk caller (the MCP tool-catalog filter, later) can evaluate many
// actions against one already-fetched grant set instead of issuing one
// membership lookup plus one permission query per action.
func ActionMatches(granted []string, action string) bool {
	resource, _, _ := strings.Cut(action, ":")
	wildcard := resource + ":*"

	for _, a := range granted {
		if a == "*" || a == action || a == wildcard {
			return true
		}
	}
	return false
}

// Principal is a resolved "who, in which org, allowed to do what" — the
// output of Service.Authorize. It carries the caller's granted action set
// (and the role, for the owner bypass) so a handler can answer many
// Allows(action) questions off of one membership lookup and one permission
// query, instead of re-querying per action.
//
// Principal must never grow a credential field (token, key, password, or
// any other secret): it is built once per request and threaded through
// bulk permission checks (e.g. filtering an MCP tool catalog), and anything
// resembling a credential on it would multiply the ways a secret could leak
// through logging, context propagation, or a future audit-log field.
type Principal struct {
	UserID         uuid.UUID
	OrganizationID uuid.UUID
	Role           string
	Actions        []string
}

// Allows reports whether p may perform action: an "owner" role bypasses the
// roles tables entirely (mirroring HasPermission's existing owner check),
// otherwise it defers to ActionMatches against p.Actions.
func (p *Principal) Allows(action string) bool {
	if p.Role == "owner" {
		return true
	}
	return ActionMatches(p.Actions, action)
}
