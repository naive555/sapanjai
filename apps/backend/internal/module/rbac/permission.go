package rbac

import (
	"strings"

	"github.com/google/uuid"
)

// ActionMatches reports whether granted permits action: "*" grants
// everything, an exact match grants that one action, and "<resource>:*"
// grants every verb on that resource. An action with no ":" wildcards to
// "<action>:*". Separate from HasPermission so a bulk caller — the MCP
// catalog filter — can evaluate many actions against one fetched grant set
// rather than one query per action.
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
// output of Service.Authorize. It carries the granted action set and the
// role (for the owner bypass) so many Allows checks cost one lookup.
//
// Principal must never grow a credential field. It is threaded through
// request context and bulk permission checks, so anything secret on it
// would multiply the ways it could leak into a log or an audit row.
type Principal struct {
	UserID         uuid.UUID
	OrganizationID uuid.UUID
	Role           string
	Actions        []string
}

// Allows reports whether p may perform action. An "owner" role bypasses the
// roles tables entirely; everyone else defers to ActionMatches.
func (p *Principal) Allows(action string) bool {
	if p.Role == "owner" {
		return true
	}
	return ActionMatches(p.Actions, action)
}

// Narrow intersects p's live RBAC grant with an MCP key's nullable scopes:
// a key can only ever narrow, never widen.
//
// scopes == nil (SQL NULL) means no independent restriction — p is returned
// unchanged, owner bypass included, exactly what a plain JWT caller gets.
//
// scopes != nil keeps the subset of the scopes list that p already permits.
// Each candidate is tested against the *un-narrowed* p, so an owner minting
// a scoped key can grant any action they hold, including ones reachable
// only via the bypass — but only those named in scopes.
//
// The returned Principal has Role == "", and that line is load-bearing: had
// it kept "owner", its own Allows would short-circuit true for every action
// regardless of Actions, silently discarding the scoping this method exists
// to enforce. Clearing Role forces Allows through ActionMatches against
// exactly the intersected set, for owners and members alike.
func (p *Principal) Narrow(scopes []string) *Principal {
	if scopes == nil {
		return p
	}

	kept := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		if p.Allows(scope) {
			kept = append(kept, scope)
		}
	}

	return &Principal{
		UserID:         p.UserID,
		OrganizationID: p.OrganizationID,
		Actions:        kept,
	}
}
