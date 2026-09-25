package auth

import (
	"strings"
	"testing"

	"github.com/go-playground/validator/v10"
)

// The password cap bounds how much input Argon2id hashes on unauthenticated
// routes. validator's max counts runes, not bytes, so a multi-byte password
// is judged by its length as the user sees it.
func TestPasswordLengthCap(t *testing.T) {
	v := validator.New(validator.WithRequiredStructEnabled())
	atCap := strings.Repeat("a", 1024)
	overCap := atCap + "a"
	multiByteAtCap := strings.Repeat("ก", 1024) // 3 bytes each

	tests := []struct {
		name    string
		req     any
		wantErr bool
	}{
		{"register at cap", RegisterRequest{Email: "a@b.co", Password: atCap}, false},
		{"register over cap", RegisterRequest{Email: "a@b.co", Password: overCap}, true},
		{"register multibyte at cap", RegisterRequest{Email: "a@b.co", Password: multiByteAtCap}, false},
		{"login at cap", LoginRequest{Email: "a@b.co", Password: atCap}, false},
		{"login over cap", LoginRequest{Email: "a@b.co", Password: overCap}, true},
		{"reset at cap", ResetPasswordRequest{Token: "t", Password: atCap}, false},
		{"reset over cap", ResetPasswordRequest{Token: "t", Password: overCap}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := v.Struct(tt.req); (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
