package server

import (
	"regexp"

	"github.com/go-playground/validator/v10"
)

// requestValidator adapts go-playground/validator to echo.Validator, so
// handlers can call echo.Context.Validate on bound request structs. Struct
// `validate` tags replace the TypeBox schemas used in the source app.
type requestValidator struct {
	v *validator.Validate
}

// orgSlugPattern mirrors OrgModel.createBody's slug pattern in the source
// app's src/modules/organization/model.ts: lowercase letters, digits, and
// hyphens only.
var orgSlugPattern = regexp.MustCompile(`^[a-z0-9-]+$`)

func newRequestValidator() *requestValidator {
	v := validator.New(validator.WithRequiredStructEnabled())
	// registered as "orgslug"; used by organization.CreateRequest.Slug.
	_ = v.RegisterValidation("orgslug", func(fl validator.FieldLevel) bool {
		return orgSlugPattern.MatchString(fl.Field().String())
	})
	return &requestValidator{v: v}
}

func (rv *requestValidator) Validate(i any) error {
	return rv.v.Struct(i)
}
