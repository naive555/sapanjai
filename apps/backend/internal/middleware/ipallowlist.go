package middleware

import (
	"net"
	"net/http"

	"github.com/labstack/echo/v4"
)

// IPAllowlist gates whatever it is applied to by c.RealIP() against allowed
// (a CIDR list parsed once at boot — see server.go's e.IPExtractor comment
// for what c.RealIP() actually depends on and does not guarantee in this
// deployment).
//
// Two callers today, both in server.go:
//
//   - the /admin route group (ADMIN_IP_ALLOWLIST, execution plan Task 6.2,
//     docs/11-admin-panel.md), applied to the GROUP so it runs before
//     RequireAuth/RequirePlatformRole — an off-network caller must not
//     reach the password/TOTP surface at all;
//   - POST /billing/webhook (STRIPE_WEBHOOK_IP_ALLOWLIST, billing plan step
//     7), applied per-route, narrowing an endpoint that by necessity has no
//     auth guard to Stripe's published egress ranges.
//
// Rejection is a plain 404 "Route not found" (the same normalization
// server.go's error handler already applies to any unmatched route), never
// 403 — a 403 would confirm to a scanner that something exists here to be
// denied from. For the webhook that also means a non-2xx, so a genuinely
// misdirected Stripe delivery is retried rather than dropped.
//
// An empty/nil allowed disables the check entirely and lets every request
// through unmodified — required for local dev, and the config-level default
// for both variables when unset.
//
// (Formerly AdminIPAllowlist. Renamed when the webhook became its second
// caller; the behaviour is unchanged.)
func IPAllowlist(allowed []*net.IPNet) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if len(allowed) == 0 {
				return next(c)
			}

			ip := net.ParseIP(c.RealIP())
			if ip == nil {
				// c.RealIP() returned something unparseable (empty, or a
				// malformed header echo's extractor couldn't resolve to an
				// address) — fail closed, the same as any IP outside the
				// list.
				return echo.NewHTTPError(http.StatusNotFound)
			}

			for _, cidr := range allowed {
				if cidr.Contains(ip) {
					return next(c)
				}
			}
			return echo.NewHTTPError(http.StatusNotFound)
		}
	}
}
