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

// Narrow intersects p's live RBAC grant with an MCP key's own (nullable)
// scopes column, implementing docs/07-sheets-adapter-plan.md Decision 1: "a
// key can only ever narrow, never widen". Lives here, next to Allows and
// ActionMatches, because it is the same permission semantics applied twice
// rather than a new concept — internal/middleware.RequireMCPKey calls it
// (via a function value injected from server.go, to avoid that package
// importing this one — see mcpkey.go's mcpPrincipalResolver doc comment)
// immediately after Authorize resolves the caller's live grant.
//
// scopes == nil (SQL NULL) means the key carries no independent
// restriction: p is returned unchanged, owner bypass included. That is
// exactly "whatever the user can do in this org, re-resolved per request" —
// the same value Authorize would have produced for a plain JWT caller.
//
// scopes != nil narrows. The effective action set becomes the subset of the
// *scopes list itself* that p already permits — each candidate is checked
// with p.Allows on the original, un-narrowed p, so an owner's bypass still
// participates in that membership test: an owner minting a scoped key is
// entitled to grant any action they hold, including ones only reachable via
// the bypass, but only the ones actually named in scopes.
//
// The returned Principal deliberately has Role == "" (never "owner"), with
// Actions set to that intersected list. This is the load-bearing line: if
// the narrowed Principal kept Role == "owner", its own Allows() would
// short-circuit to true for *every* action regardless of Actions, and the
// scoping this method exists to enforce would be silently discarded — a
// scoped key minted by an owner would grant everything, not just its
// scopes. Clearing Role forces the narrowed Principal's Allows() to fall
// through to ActionMatches against exactly the intersected set, for owners
// and members alike.
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
