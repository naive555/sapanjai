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
	"github.com/sapanjai/backend/internal/module/billing"
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
	"github.com/sapanjai/backend/internal/shared/password"
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
	// /admin ADMIN_IP_ALLOWLIST check (internal/middleware.IPAllowlist)
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

	if err := password.Configure(cfg.PasswordHashing); err != nil {
		return nil, fmt.Errorf("password hashing: %w", err)
	}
	ph := cfg.PasswordHashing
	log.Info("password hashing configured",
		slog.Uint64("memory_kib", uint64(ph.MemoryKiB)),
		slog.Uint64("iterations", uint64(ph.Iterations)),
		slog.Int("parallelism", int(ph.Parallelism)),
		slog.Int("max_concurrent", ph.MaxConcurrent),
		slog.Uint64("peak_memory_mib", uint64(ph.MemoryKiB)*uint64(ph.MaxConcurrent)/1024))

	authSvc := auth.NewService(store, redisAuth, auditSvc, redisEmail, renderer, cfg.AppPublicURL, log)
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
	// subSvc (constructed above for /subscription and connector's own
	// max_connectors check) is reused as-is for the gateway's
	// max_tool_calls_per_month quota check (step 5 of
	// .claude/plans/2026-09-13-billing-and-usage-metering.md) — the same
	// EnforceLimit method, no new subscription plumbing.
	mcpSvc := mcp.NewService(connectorSvc, mcpLimiter, auditSvc, store, subSvc, log, cfg.ConnectorMasterKey)
	mcp.NewHandler(mcpSvc, log).Register(e.Group("/mcp"), appmw.RequireMCPKey(store, resolveMCPPrincipal, log))

	// Billing (step 6 of
	// .claude/plans/2026-09-13-billing-and-usage-metering.md). Both routes
	// sit on RequirePermission("billing:write"), never RequireOrg — see
	// billing.PermissionWrite.
	//
	// newStripeClient returns nil when STRIPE_SECRET_KEY is unset, and the
	// routes are mounted anyway: they stay permission-guarded and answer
	// BILLING_NOT_CONFIGURED (501). Mounting unconditionally is what keeps a
	// Stripe-less local dev box and a production deployment presenting the
	// same route surface, so a guard regression cannot hide behind a route
	// that simply isn't there. This is also the one secret the API holds
	// that the RESEND_API_KEY precedent would have kept on the worker:
	// checkout creation is request-driven, so a restricted key (rk_) bounds
	// the blast radius instead — see config.Config.StripeSecretKey.
	//
	// cfg.AppPublicURL, not this API's address: every URL Stripe redirects a
	// human to is a page in apps/frontend.
	//
	// subSvc is injected as billing's narrow planAssigner seam, following
	// internal/module/admin's subscriptionResolver: the org_subscriptions
	// upsert has exactly one implementation and billing does not grow a
	// second (plan invariant 5). It is also injected a second time as the
	// limitResolver seam GET /billing/usage reads through (billing plan
	// step 9) — one *subscription.Service instance satisfying two narrow,
	// single-method interfaces, rather than billing widening either one
	// into something a reader has to trace back to figure out which half
	// is actually used where.
	billingSvc := billing.NewService(
		store,
		billing.NewStripeClient(cfg.StripeSecretKey),
		billing.NewStripeWebhooks(cfg.StripeWebhookSecret),
		subSvc,
		subSvc,
		auditSvc,
		cfg.AppPublicURL,
		log,
	)
	billingHandler := billing.NewHandler(billingSvc)
	billingHandler.Register(e.Group("/billing"), guards)

	// POST /billing/webhook (step 7) is mounted OUTSIDE that group and with
	// no auth guard of any kind, because Stripe presents neither a JWT nor
	// an x-organization-id — a webhook behind RequireAuth/RequireOrg/
	// RequirePermission could never be reached by Stripe at all. Its
	// authentication is the Stripe-Signature HMAC, verified against the raw
	// request body inside the handler.
	//
	// That raw-body verification is the reason the global middleware stack
	// above is exactly Recover + RequestID + requestLogger: none of them
	// reads a request body. Anything added globally that does would consume
	// the reader and make every signature check fail. Do not add one.
	//
	// IPAllowlist is the same middleware /admin uses, here narrowing the
	// route to Stripe's published egress ranges — defence in depth behind
	// the signature, never instead of it, and disabled by default because a
	// stale CIDR list silently drops live billing events (see
	// config.Config.StripeWebhookIPAllowlist).
	billingHandler.RegisterWebhook(e, appmw.IPAllowlist(cfg.StripeWebhookIPAllowlist))

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
	// IPAllowlist (Phase 6 Task 6.2) is group middleware, so it runs
	// BEFORE guards.RequirePlatformRole/RequirePlatformRoleNo2FA on every
	// route below — an off-network request never reaches RequireAuth, let
	// alone the platform-role or 2FA checks. See its own doc comment for
	// the 404-not-403 reasoning and e.IPExtractor above for what c.RealIP()
	// actually guarantees in this deployment.
	admin.NewHandler(adminSvc).Register(e.Group("/admin", appmw.IPAllowlist(cfg.AdminIPAllowlist)), guards)

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
		LogStatus:       true,
		LogURI:          true,
		LogMethod:       true,
		LogLatency:      true,
		LogRequestID:    true,
		LogRoutePath:    true,
		LogResponseSize: true,
		LogError:        true,
		// Deliberately absent: LogHeaders, LogQueryParams, LogFormValues. The
		// centralized redaction in internal/shared/logger matches attribute
		// keys, so an Authorization header nested inside a map value would
		// reach the log untouched.
		//
		// Required for an accurate status: without it the middleware reads
		// res.Status before the error handler has run (so every errored
		// request logs 200), and its only fallback unwraps *echo.HTTPError —
		// which misses the *apperror.Error that every service returns.
		HandleError: true,
		LogValuesFunc: func(c echo.Context, v echomw.RequestLoggerValues) error {
			uri := logger.SanitizeURI(v.URI)
			attrs := []any{
				"method", v.Method,
				"uri", uri,
				// The matched pattern (/connectors/:id), not the filled path —
				// uri is unique per request and cannot be grouped or alerted on.
				"route", v.RoutePath,
				"status", v.Status,
				"latency", v.Latency.String(),
				// Alongside the string, not instead of it: "7.221966ms" cannot
				// be sorted or thresholded by a log backend.
				"latency_ms", float64(v.Latency.Nanoseconds()) / 1e6,
				"request_id", v.RequestID,
				"bytes_out", v.ResponseSize,
			}

			// Absent on unauthenticated routes; the getters are comma-ok and
			// return the zero uuid rather than panicking.
			if userID := appmw.UserID(c); userID != uuid.Nil {
				attrs = append(attrs, "user_id", userID.String())
			}
			if orgID := appmw.OrgID(c); orgID != uuid.Nil {
				attrs = append(attrs, "org_id", orgID.String())
			}
			if appmw.IsImpersonated(c) {
				attrs = append(attrs, "impersonated", true, "actor_id", appmw.ActorID(c).String())
			}

			if v.Error != nil {
				var appErr *apperror.Error
				if asAppError(v.Error, &appErr) {
					attrs = append(attrs, "error_code", appErr.Code)
				}
				// The message only above 500, where the code is absent or
				// generic and the cause lives solely in the wrapped error.
				// A 4xx is already named by its code, and an infra error's
				// message can quote credential-adjacent upstream detail.
				if v.Status >= 500 {
					attrs = append(attrs, "error", v.Error.Error())
				}
			}

			log.Info("request", attrs...)
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
