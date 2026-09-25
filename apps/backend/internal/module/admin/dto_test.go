package admin

import (
	"strings"
	"testing"

	"github.com/go-playground/validator/v10"
)

func TestReauthPasswordLengthCap(t *testing.T) {
	v := validator.New(validator.WithRequiredStructEnabled())
	atCap := strings.Repeat("a", 1024)
	overCap := atCap + "a"

	tests := []struct {
		name    string
		req     any
		wantErr bool
	}{
		{"delete org at cap", DeleteOrganizationRequest{Confirm: "acme", Password: atCap}, false},
		{"delete org over cap", DeleteOrganizationRequest{Confirm: "acme", Password: overCap}, true},
		{"platform role at cap", PlatformRoleRequest{Password: atCap}, false},
		{"platform role over cap", PlatformRoleRequest{Password: overCap}, true},
		{"ban at cap", BanRequest{Banned: true, Password: atCap}, false},
		{"ban over cap", BanRequest{Banned: true, Password: overCap}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := v.Struct(tt.req); (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
