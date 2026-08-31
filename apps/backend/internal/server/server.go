// Package server wires the Echo instance: middleware, error handling, and
// route registration for the full API.
package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"
	"github.com/redis/go-redis/v9"
	echoSwagger "github.com/swaggo/echo-swagger"

	_ "github.com/sapanjai/backend/docs" // generated OpenAPI spec
	"github.com/sapanjai/backend/internal/adapter/googlesheets"
	"github.com/sapanjai/backend/internal/config"
	"github.com/sapanjai/backend/internal/infra/database"
	appredis "github.com/sapanjai/backend/internal/infra/redis"
	appmw "github.com/sapanjai/backend/internal/middleware"
	"github.com/sapanjai/backend/internal/module/admin"
	"github.com/sapanjai/backend/internal/module/auditlog"
	"github.com/sapanjai/backend/internal/module/auth"
	"github.com/sapanjai/backend/internal/module/connector"
	"github.com/sapanjai/backend/internal/module/health"
	"github.com/sapanjai/backend/internal/module/mcp"
	"github.com/sapanjai/backend/internal/module/mcpkey"
	"github.com/sapanjai/backend/internal/module/organization"
	"github.com/sapanjai/backend/internal/module/rbac"
	"github.com/sapanjai/backend/internal/module/subscription"
	"github.com/sapanjai/backend/internal/shared/apperror"
	"github.com/sapanjai/backend/internal/shared/email"
	"github.com/sapanjai/backend/internal/shared/envelope"
	"github.com/sapanjai/backend/internal/shared/httpx"
	"github.com/sapanjai/backend/internal/shared/logger"
)

// New builds a fully configured Echo instance: middleware stack, custom
// error handler, infra-backed module wiring, and route registration. It
// returns an error when wiring that depends on validated-but-fallible
// configuration fails (today: the connector master key).
func New(cfg *config.Config, log *slog.Logger, pool *pgxpool.Pool, rdb *redis.Client) (*echo.Echo, error) {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	// e.IPExtractor governs c.RealIP() everywhere it's read: the
	// /admin ADMIN_IP_ALLOWLIST check (internal/middleware.AdminIPAllowlist)
	// and the ip field in every admin audit entry
	// (internal/module/admin/handler.go's adminContext). Echo's default
	// (no IPExtractor set) trusts X-Forwarded-For unconditionally, which is
	// exactly the hole an off-network caller would use to both fabricate
	// audit evidence and walk through the allowlist by claiming to be an
	// allowed address.
	//
	// TrustLoopback/TrustPrivateNet cover the two hops this deployment
	// actually has (docs/09-railway-deploy.md): Railway's edge reaches a
	// container over its private network (IPv6 ULA — "IPv6-only private
	// network" in that doc), and apps/frontend's own proxy
	// (app/api/[...path]/route.ts) reaches this API the same way in
	// docker-compose/k8s. Echo walks the X-Forwarded-For chain from the
	// right, skipping every hop in those trusted ranges, and returns the
	// first entry that isn't — see the vendored
	// github.com/labstack/echo/v4/ip.go for the exact algorithm.
	//
	// The half this does NOT solve by itself: route.ts used to forward an
	// INBOUND X-Forwarded-For/X-Real-IP verbatim, so a caller could inject
	// a fabricated chain before it ever reached this extractor — no amount
	// of trust-range configuration here can tell a forged chain from a
	// genuine one after the fact once attacker input and proxy input share
	// the same header. route.ts now strips both headers unconditionally on
	// every request (see its own comment) rather than trying to sanitize
	// them, which is the fix ip.go's own doc explicitly calls for: "never
	// forget to configure the outermost proxy... not to pass through
	// incoming headers."
	//
	// The consequence worth stating plainly (also spelled out in
	// docs/09-railway-deploy.md's "Admin console" section): for every
	// request that reaches this API through the frontend proxy — which is
	// 100% of the admin console's browser traffic, since it is same-origin
	// through /api/admin/* like every other page — c.RealIP() now resolves
	// to the FRONTEND'S OWN private-network address, not the staff member's
	// real one. There is no hop left standing between "attacker-controlled"
	// and this API that could carry that information trustworthily without
	// either (a) this API trusting a header set by Railway's edge for the
	// browser-facing hop into the frontend, which cannot be verified from
	// this codebase and is deliberately not assumed, or (b) the admin
	// console bypassing the frontend proxy entirely, which is a bigger
	// architecture change outside this phase's scope. ADMIN_IP_ALLOWLIST is
	// therefore a real control against internet-wide scanning of this API's
	// public /admin/* routes (its stated purpose — Task 6.2's off-network
	// scanner gets a 404), but it is NOT a per-staff-office/VPN control for
	// traffic through the console's normal path: an operator who needs that
	// granularity must enforce it at the platform edge in front of the
	// frontend (Railway access control, a WAF rule, or a VPN requirement),
	// not through this backend. This is the honest state of the control,
	// not a bug to fix in a later phase.
	e.IPExtractor = echo.ExtractIPFromXFFHeader(echo.TrustLoopback(true), echo.TrustPrivateNet(true))

	e.HTTPErrorHandler = newErrorHandler(log)
	e.Validator = newRequestValidator()

	e.Use(echomw.Recover())
	e.Use(appmw.RequestID())
	e.Use(requestLogger(log))

	health.NewHandler().Register(e)

	e.GET("/swagger", func(c echo.Context) error {
		return c.Redirect(http.StatusMovedPermanently, "/swagger/index.html")
	})
	e.GET("/swagger/*", echoSwagger.WrapHandler)

	store := database.NewStore(pool)
	redisAuth := appredis.NewAuth(rdb, cfg.RedisKeyPrefix)
	redisEmail := appredis.NewEmail(rdb, cfg.RedisKeyPrefix)
	tokenSvc := auth.NewTokenService(cfg)
	auditSvc := auditlog.NewService(store, log)

	// The API only ever enqueues verification mail (email_outbox); it never
	// talks to Resend itself (internal/job/emaildispatch, run by the
	// worker, is the only sender) — see CLAUDE.md's Background worker
	// bullet. NewRenderer parses the embedded templates once at boot so a
	// malformed template fails startup rather than the first registration.
	renderer, err := email.NewRenderer()
	if err != nil {
		return nil, fmt.Errorf("email renderer: %w", err)
	}

	rbacSvc := rbac.NewService(store)
	guards := appmw.NewGuards(tokenSvc, redisAuth, store, rbacSvc)

	authSvc := auth.NewService(store, redisAuth, auditSvc, redisEmail, renderer, cfg.AppPublicURL)
	authHandler := auth.NewHandler(authSvc, tokenSvc, store, redisAuth, cfg.JWTRefreshExpiresIn)
	authHandler.Register(e.Group("/auth"), guards)

	subSvc := subscription.NewService(store)
	orgSvc := organization.NewService(store, auditSvc, subSvc)
	orgHandler := organization.NewHandler(orgSvc)
	orgHandler.Register(e.Group("/organizations"), guards)

	rbac.NewHandler(rbacSvc).Register(e.Group("/rbac"), guards)
	subHandler := subscription.NewHandler(subSvc)
	subHandler.Register(e.Group("/subscription"), guards)
	subHandler.RegisterPlans(e.Group("/plans"), guards)
	auditlog.NewHandler(auditSvc).Register(e.Group("/audit-logs"), guards)

	keyProvider, err := envelope.NewEnvKeyProvider(cfg.ConnectorMasterKey, cfg.ConnectorMasterKeysRetired...)
	if err != nil {
		return nil, fmt.Errorf("connector master key: %w", err)
	}
	// One Encryptor instance, shared by connectorSvc and adminSvc: a
	// user_totp.secret_encrypted row is sealed under the exact same
	// CONNECTOR_MASTER_KEY machinery a connector's config is (Phase 6 Task
	// 6.3 — "no new secret is introduced"), so rotation is already solved
	// and there is nothing TOTP-specific to configure here.
	crypto := envelope.New(keyProvider)
	// googlesheets.NewChecker is the first real health-check adapter
	// (docs/07-sheets-adapter-decisions.md step 5); every other connector type
	// still resolves to 501 HEALTH_CHECK_UNSUPPORTED with no Checker
	// registered.
	connectorSvc := connector.NewService(store, crypto, auditSvc, subSvc, connector.NewRegistry(googlesheets.NewChecker()), log)
	connector.NewHandler(connectorSvc).Register(e.Group("/connectors"), guards)

	mcpKeySvc := mcpkey.NewService(store, log)
	mcpkey.NewHandler(mcpKeySvc).Register(e.Group("/mcp-keys"), guards)

	// The MCP gateway (docs/07-sheets-adapter-decisions.md step 3) uses a
	// different credential than every other route: a long-lived PAT
	// (mcp_api_keys), not the JWT pair RequireAuth/RequireOrg/
	// RequirePermission verify. RequireMCPKey re-resolves the caller's live
	// RBAC grant via rbacSvc.Authorize on every request rather than trusting
	// anything cached on the key itself, then narrows it by the key's own
	// scopes. This closure — not a direct method value — is what lets
	// internal/middleware avoid importing internal/module/rbac (an import
	// cycle; see appmw.MCPPrincipalResolver's doc comment): server.go
	// already imports both, so it is the natural place to compose them.
	resolveMCPPrincipal := func(ctx context.Context, userID, organizationID uuid.UUID, scopes []string) (any, error) {
		principal, err := rbacSvc.Authorize(ctx, userID, organizationID)
		if err != nil {
			return nil, err
		}
		return principal.Narrow(scopes), nil
	}
	mcpLimiter := appredis.NewRateLimiter(rdb, cfg.MCPRateLimitPerMin, cfg.RedisKeyPrefix)
	mcpSvc := mcp.NewService(connectorSvc, mcpLimiter, auditSvc, log, cfg.ConnectorMasterKey)
	mcp.NewHandler(mcpSvc, log).Register(e.Group("/mcp"), appmw.RequireMCPKey(store, resolveMCPPrincipal, log))

	// The admin console (docs/11-admin-panel.md) sits outside the tenant
	// boundary: RequirePlatformRole, not RequireOrg/RequirePermission.
	// redisAuth is reused as-is for the reauth rate limiter and the
	// durable-ban cache (D3) — the same *redis.Auth every login-path check
	// already goes through. tokenSvc is the same signer the /auth module
	// uses: an impersonation token is an ordinary access token carrying
	// imp/act claims, so it must verify under the very same secret the
	// guards already check, not a parallel one.
	adminCache := appredis.NewAdminCount(rdb, cfg.RedisKeyPrefix)
	adminSvc := admin.NewService(store, adminCache, auditSvc, subSvc, redisAuth, tokenSvc, crypto, log)
	// SetAdminRequire2FA wires ADMIN_REQUIRE_2FA into RequirePlatformRole
	// (Phase 6 Task 6.3) — a setter rather than a NewGuards parameter so
	// every other module's existing test call sites don't need updating for
	// one admin-only boolean; see Guards.adminRequire2FA's doc comment.
	guards.SetAdminRequire2FA(cfg.AdminRequire2FA)
	// AdminIPAllowlist (Phase 6 Task 6.2) is group middleware, so it runs
	// BEFORE guards.RequirePlatformRole/RequirePlatformRoleNo2FA on every
	// route below — an off-network request never reaches RequireAuth, let
	// alone the platform-role or 2FA checks. See its own doc comment for
	// the 404-not-403 reasoning and e.IPExtractor above for what c.RealIP()
	// actually guarantees in this deployment.
	admin.NewHandler(adminSvc).Register(e.Group("/admin", appmw.AdminIPAllowlist(cfg.AdminIPAllowlist)), guards)

	return e, nil
}

// newErrorHandler returns Echo's global error handler. It maps:
//   - *apperror.Error   -> apperror.Resolve(code)
//   - *echo.HTTPError    -> its status/message (404 normalized to "Route not found";
//     handlers use httpx.BindAndValidate to produce 400 "Invalid request body" /
//     422 "Validation failed" as *echo.HTTPError)
//   - anything else      -> logged at error level, 500 "Internal server error"
func newErrorHandler(log *slog.Logger) echo.HTTPErrorHandler {
	return func(err error, c echo.Context) {
		if c.Response().Committed {
			return
		}

		status := http.StatusInternalServerError
		message := "Internal server error"

		var appErr *apperror.Error
		var httpErr *echo.HTTPError

		switch {
		case asAppError(err, &appErr):
			status, message = apperror.Resolve(appErr.Code)

		case asHTTPError(err, &httpErr):
			status = httpErr.Code
			if status == http.StatusNotFound {
				message = "Route not found"
			} else if msg, ok := httpErr.Message.(string); ok {
				message = msg
			}

		default:
			log.Error("unhandled error", "error", err, "path", c.Request().URL.Path)
		}

		if status >= 500 {
			log.Error("request failed", "error", err, "status", status, "path", c.Request().URL.Path)
		}

		if writeErr := c.JSON(status, httpx.ErrorResponse{Message: message}); writeErr != nil {
			log.Error("failed to write error response", "error", writeErr)
		}
	}
}

func asAppError(err error, target **apperror.Error) bool {
	if e, ok := err.(*apperror.Error); ok {
		*target = e
		return true
	}
	return false
}

func asHTTPError(err error, target **echo.HTTPError) bool {
	if e, ok := err.(*echo.HTTPError); ok {
		*target = e
		return true
	}
	return false
}

// requestLogger logs each request at info level via slog. The URI is passed
// through logger.SanitizeURI so a secret smuggled in as a query parameter is
// censored; headers and bodies are never logged at all.
func requestLogger(log *slog.Logger) echo.MiddlewareFunc {
	return requestLoggerWithSink(log, nil)
}

// requestLoggerWithSink is requestLogger with a test hook receiving the same
// status and sanitized URI that reach the log line.
func requestLoggerWithSink(log *slog.Logger, sink func(status int, uri string)) echo.MiddlewareFunc {
	return echomw.RequestLoggerWithConfig(echomw.RequestLoggerConfig{
		LogStatus:    true,
		LogURI:       true,
		LogMethod:    true,
		LogLatency:   true,
		LogRequestID: true,
		// Required for an accurate status: without it the middleware reads
		// res.Status before the error handler has run (so every errored
		// request logs 200), and its only fallback unwraps *echo.HTTPError —
		// which misses the *apperror.Error that every service returns.
		HandleError: true,
		LogValuesFunc: func(c echo.Context, v echomw.RequestLoggerValues) error {
			uri := logger.SanitizeURI(v.URI)
			log.Info("request",
				"method", v.Method,
				"uri", uri,
				"status", v.Status,
				"latency", v.Latency.String(),
				"request_id", v.RequestID,
			)
			if sink != nil {
				sink(v.Status, uri)
			}
			return nil
		},
	})
}

// Shutdown gracefully stops the server and closes infrastructure clients.
func Shutdown(ctx context.Context, e *echo.Echo, pool *pgxpool.Pool, rdb *redis.Client) error {
	if err := e.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown echo: %w", err)
	}
	pool.Close()
	if err := rdb.Close(); err != nil {
		return fmt.Errorf("close redis: %w", err)
	}
	return nil
}
