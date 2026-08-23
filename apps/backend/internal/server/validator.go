package server

import (
	"regexp"

	"github.com/go-playground/validator/v10"

	"github.com/sapanjai/backend/internal/module/connector"
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

// permActionPattern matches a single RBAC action string, the same shape
// ActionMatches (internal/module/rbac/permission.go) already understands:
// the bare wildcard "*", an exact "resource:verb", or a resource-scoped
// "resource:*". resource and verb are each a lowercase identifier —
// letters/digits/underscore, starting with a letter — mirroring how actions
// are seeded (rbac roles, connector/mcpkey PermissionRead-style constants)
// rather than accepting arbitrary punctuation no seeded action ever uses.
var permActionPattern = regexp.MustCompile(`^(\*|[a-z][a-z0-9_]*:(\*|[a-z][a-z0-9_]*))$`)

func newRequestValidator() *requestValidator {
	v := validator.New(validator.WithRequiredStructEnabled())
	// registered as "orgslug"; used by organization.CreateRequest.Slug.
	_ = v.RegisterValidation("orgslug", func(fl validator.FieldLevel) bool {
		return orgSlugPattern.MatchString(fl.Field().String())
	})
	// registered as "connectortype"; used by connector.CreateRequest.Type.
	// The valid set lives in the connector module so adding a type is one
	// constant there, not a tag edit here.
	_ = v.RegisterValidation("connectortype", func(fl validator.FieldLevel) bool {
		return connector.IsValidType(fl.Field().String())
	})
	// registered as "permaction"; used by mcpkey.CreateRequest.Scopes (one
	// tag application per element via `dive`). Deliberately not validated
	// against the caller's live grant here or anywhere else at mint time —
	// see docs/08-gateway-core.md §4 and mcpkey.Service.Create's doc
	// comment — this only rejects a string that could never be a valid
	// action, not one the caller happens not to hold right now.
	_ = v.RegisterValidation("permaction", func(fl validator.FieldLevel) bool {
		return permActionPattern.MatchString(fl.Field().String())
	})
	return &requestValidator{v: v}
}

func (rv *requestValidator) Validate(i any) error {
	return rv.v.Struct(i)
}
