package server

import "testing"

type orgSlugFixture struct {
	Slug string `validate:"required,min=2,orgslug"`
}

func TestOrgSlugValidation(t *testing.T) {
	v := newRequestValidator()

	tests := []struct {
		name    string
		slug    string
		wantErr bool
	}{
		{"valid lowercase", "acme-corp", false},
		{"valid digits and hyphens", "team-42", false},
		{"valid single char repeated", "aa", false},
		{"uppercase rejected", "Acme-Corp", true},
		{"underscore rejected", "acme_corp", true},
		{"space rejected", "acme corp", true},
		{"too short", "a", true},
		{"empty", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v.Validate(&orgSlugFixture{Slug: tt.slug})
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate(%q) error = %v, wantErr %v", tt.slug, err, tt.wantErr)
			}
		})
	}
}
