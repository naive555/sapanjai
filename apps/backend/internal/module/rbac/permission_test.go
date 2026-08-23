package rbac

import (
	"testing"

	"github.com/google/uuid"
)

// ---- ActionMatches ----

func TestActionMatches(t *testing.T) {
	cases := []struct {
		name    string
		granted []string
		action  string
		want    bool
	}{
		{"star grants anything", []string{"*"}, "billing:delete", true},
		{"exact match", []string{"project:create"}, "project:create", true},
		{"exact no match", []string{"project:create"}, "project:read", false},
		{"resource wildcard matches same resource", []string{"project:*"}, "project:create", true},
		{"resource wildcard does not match other resource", []string{"invoice:*"}, "connector:read", false},
		{"empty granted set denies", []string{}, "project:read", false},
		{"nil granted set denies", nil, "project:read", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ActionMatches(tc.granted, tc.action)
			if got != tc.want {
				t.Errorf("ActionMatches(%v, %q) = %v, want %v", tc.granted, tc.action, got, tc.want)
			}
		})
	}
}

// ---- Principal.Allows ----

func TestPrincipal_Allows(t *testing.T) {
	cases := []struct {
		name    string
		role    string
		actions []string
		action  string
		want    bool
	}{
		{"owner bypasses even with no actions", "owner", nil, "anything:at-all", true},
		{"owner bypasses with empty action set", "owner", []string{}, "connector:delete", true},
		{"member star grants anything", "member", []string{"*"}, "billing:delete", true},
		{"member exact match", "member", []string{"project:create"}, "project:create", true},
		{"member resource wildcard", "member", []string{"project:*"}, "project:create", true},
		{"member wildcard does not leak to other resource", "member", []string{"invoice:*"}, "connector:read", false},
		{"member empty actions denies", "member", []string{}, "project:read", false},
		{"member nil actions denies", "member", nil, "project:read", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Principal{
				UserID:         uuid.New(),
				OrganizationID: uuid.New(),
				Role:           tc.role,
				Actions:        tc.actions,
			}
			got := p.Allows(tc.action)
			if got != tc.want {
				t.Errorf("Principal{Role: %q, Actions: %v}.Allows(%q) = %v, want %v", tc.role, tc.actions, tc.action, got, tc.want)
			}
		})
	}
}

// ---- Principal.Narrow ----
//
// docs/07-sheets-adapter-decisions.md Decision 1: an MCP key's scopes intersect
// the live RBAC grant and can only ever narrow it, never widen it.

func TestPrincipal_Narrow_NilScopesLeavesPrincipalUnchanged(t *testing.T) {
	userID, orgID := uuid.New(), uuid.New()
	p := &Principal{UserID: userID, OrganizationID: orgID, Role: "owner"}

	got := p.Narrow(nil)

	if got != p {
		t.Fatalf("Narrow(nil) returned a different *Principal; want the same pointer (unchanged)")
	}
	if !got.Allows("anything:at-all") {
		t.Errorf("owner bypass lost after Narrow(nil); NULL scopes must leave the principal exactly as Authorize resolved it")
	}
}

func TestPrincipal_Narrow_MemberIntersection(t *testing.T) {
	p := &Principal{
		UserID:         uuid.New(),
		OrganizationID: uuid.New(),
		Role:           "member",
		Actions:        []string{"connector:read", "connector:write"},
	}

	got := p.Narrow([]string{"connector:read", "mcpkey:write"})

	if !got.Allows("connector:read") {
		t.Error("narrowed principal should still allow connector:read (in both the grant and the scopes)")
	}
	if got.Allows("connector:write") {
		t.Error("narrowed principal must not allow connector:write: it's in the grant but not in scopes")
	}
	if got.Allows("mcpkey:write") {
		t.Error("narrowed principal must not allow mcpkey:write: it's in scopes but not in the grant")
	}
}

// TestPrincipal_Narrow_OwnerBypassDoesNotSurviveScoping is the specific
// case docs/07-sheets-adapter-decisions.md step 3 calls out by name: "An owner's
// bypass must also be narrowed by a non-NULL scopes list, otherwise a
// scoped key held by an owner silently grants everything."
//
// The mechanism under test: Narrow evaluates each candidate scope against
// the *original* principal (so the owner bypass correctly says "yes, I may
// grant this" for every scope named), but the *returned* principal has
// Role cleared, so its own Allows() cannot fall back to the bypass for
// actions outside the intersected set.
func TestPrincipal_Narrow_OwnerBypassDoesNotSurviveScoping(t *testing.T) {
	owner := &Principal{UserID: uuid.New(), OrganizationID: uuid.New(), Role: "owner"}

	got := owner.Narrow([]string{"connector:read"})

	if got.Role == "owner" {
		t.Fatal("narrowed principal must not keep Role == \"owner\" — that would make Allows() bypass scoping entirely")
	}
	if !got.Allows("connector:read") {
		t.Error("narrowed principal should allow connector:read: it's in scopes, and the owner bypass permits granting it")
	}
	if got.Allows("connector:write") {
		t.Fatal("narrowed owner-held key must NOT allow connector:write: it is outside scopes, even though the owner " +
			"bypass would allow it unscoped — a scoped key must only ever narrow, never widen")
	}
	if got.Allows("billing:delete") {
		t.Fatal("narrowed owner-held key must not silently grant everything via a surviving owner bypass")
	}
}

func TestPrincipal_Narrow_ScopeWiderThanGrantIsDropped(t *testing.T) {
	// A member whose role only grants connector:read cannot use a scoped
	// key to reach further, even if the key's scopes ask for more —
	// scoping can only shrink the live grant, never substitute for it.
	p := &Principal{
		UserID:         uuid.New(),
		OrganizationID: uuid.New(),
		Role:           "member",
		Actions:        []string{"connector:read"},
	}

	got := p.Narrow([]string{"connector:*"})

	if got.Allows("connector:write") {
		t.Fatal("scopes requesting a wildcard the underlying grant does not hold must not be honored")
	}
}
