// Package principal resolves a bearer token into an rbac.Principal.
//
// In the spike this is a hardcoded table. In the real gateway it is exactly
// two calls that controlplane already has:
//
//	userID, email, err := tokenService.VerifyAccessToken(tok)   // internal/module/auth
//	actions, err := store.ListPermissionActionsByUserOrg(...)   // internal/module/rbac
//
// Keeping the seam this narrow is the point: the MCP layer never learns how
// authentication works, only how to ask.
package principal

import (
	"errors"
	"strings"

	"github.com/junctera/spikes/mcp-gateway/internal/mockdata"
	"github.com/junctera/spikes/mcp-gateway/internal/rbac"
)

// ErrUnknownToken is returned for a token with no matching principal. The
// caller maps it to HTTP 401.
var ErrUnknownToken = errors.New("unknown or expired token")

// fixtures covers the four cases worth demonstrating: owner bypass, a
// read-only grant, a resource wildcard, and a second tenant.
var fixtures = map[string]rbac.Principal{
	// Owner: HasPermission short-circuits true, sees every tool, regardless
	// of the (empty) Actions set.
	"tok_owner_siam": {
		UserID: "usr_aaaa1111",
		Email:  "owner@siamtrading.co.th",
		OrgID:  mockdata.OrgSiamTrading,
		Role:   "owner",
	},
	// Read-only analyst: exact-match grant. Sees the two read tools, not
	// create_invoice.
	"tok_reader_siam": {
		UserID:  "usr_bbbb2222",
		Email:   "analyst@siamtrading.co.th",
		OrgID:   mockdata.OrgSiamTrading,
		Role:    "member",
		Actions: []string{"invoice:read", "report:read"},
	},
	// Resource wildcard: "invoice:*" matches both invoice:read and
	// invoice:write via the wildcard branch, so this member sees all three
	// tools without being an owner.
	"tok_bookkeeper_siam": {
		UserID:  "usr_cccc3333",
		Email:   "bookkeeper@siamtrading.co.th",
		OrgID:   mockdata.OrgSiamTrading,
		Role:    "member",
		Actions: []string{"invoice:*"},
	},
	// Second tenant, same permission shape as the reader. Proves org
	// isolation: identical tools, disjoint data.
	"tok_reader_bkl": {
		UserID:  "usr_dddd4444",
		Email:   "ops@bangkoklogistics.co.th",
		OrgID:   mockdata.OrgBangkokLogi,
		Role:    "member",
		Actions: []string{"invoice:read"},
	},
	// No invoice grants at all: authenticates fine, sees zero tools.
	"tok_nogrants_siam": {
		UserID:  "usr_eeee5555",
		Email:   "intern@siamtrading.co.th",
		OrgID:   mockdata.OrgSiamTrading,
		Role:    "member",
		Actions: []string{"report:read"},
	},
}

// Resolve maps a raw bearer token to its principal.
func Resolve(token string) (*rbac.Principal, error) {
	p, ok := fixtures[strings.TrimSpace(token)]
	if !ok {
		return nil, ErrUnknownToken
	}
	return &p, nil
}

// Tokens lists the demo tokens, for the HTTP server's startup banner.
func Tokens() []string {
	return []string{"tok_owner_siam", "tok_reader_siam", "tok_bookkeeper_siam", "tok_reader_bkl", "tok_nogrants_siam"}
}
