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
