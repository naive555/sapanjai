// Package authtoken holds the verified-access-token value type shared by
// the token service that produces it (internal/module/auth) and the guards
// that consume it (internal/middleware).
//
// It exists as its own package for one reason: internal/module/auth already
// imports internal/middleware (auth.Handler reads appmw.UserID), so the
// guards' tokenVerifier interface cannot name a type declared in auth
// without an import cycle. A neutral package breaks it without inverting
// the existing dependency direction.
package authtoken

import "github.com/google/uuid"

// AccessToken is a verified access token's claims. It is a struct rather
// than a widening list of positional returns because the impersonation
// claims (docs/11-admin-panel.md §5) brought the count to four: every
// caller now names what it reads, and the next claim added here does not
// churn the signature again.
type AccessToken struct {
	UserID uuid.UUID
	Email  string

	// ActorID is the platform-staff user who started an impersonation
	// (RFC 8693's "act" claim), or uuid.Nil on an ordinary token. Only
	// meaningful when Impersonated is true.
	ActorID uuid.UUID

	// Impersonated is true for a token minted by
	// POST /admin/users/:userId/impersonate. The auth guard refuses any
	// unsafe HTTP method on such a token, and RequirePlatformRole refuses
	// it outright.
	Impersonated bool
}
