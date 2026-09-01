package middleware

import (
	"net"
	"net/http"

	"github.com/labstack/echo/v4"
)

// AdminIPAllowlist gates the /admin route group by c.RealIP() against
// allowed (config.Config.AdminIPAllowlist, parsed once at boot — see its
// own doc comment for the ADMIN_IP_ALLOWLIST env var, and server.go's
// e.IPExtractor comment for what c.RealIP() actually depends on and does
// not guarantee in this deployment).
//
// Applied to the group BEFORE RequireAuth/RequirePlatformRole — execution
// plan Task 6.2's explicit requirement: an off-network caller must not
// reach the password/TOTP surface at all, only a 404. Rejection is a plain
// 404 "Route not found" (the same normalization server.go's error handler
// already applies to any unmatched route), never 403 — a 403 would confirm
// to an off-network scanner that something exists here to be denied from.
//
// An empty/nil allowed disables the check entirely and lets every request
// through unmodified — required for local dev, and the config-level default
// when ADMIN_IP_ALLOWLIST is unset.
func AdminIPAllowlist(allowed []*net.IPNet) echo.MiddlewareFunc {
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
