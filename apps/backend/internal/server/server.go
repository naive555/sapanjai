// Package server wires the Echo instance: middleware, error handling, and
// route registration for the full API.
package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"
	"github.com/redis/go-redis/v9"
	echoSwagger "github.com/swaggo/echo-swagger"

	_ "github.com/sapanjai/backend/docs" // generated OpenAPI spec
	"github.com/sapanjai/backend/internal/config"
	"github.com/sapanjai/backend/internal/infra/database"
	appredis "github.com/sapanjai/backend/internal/infra/redis"
	appmw "github.com/sapanjai/backend/internal/middleware"
	"github.com/sapanjai/backend/internal/module/auditlog"
	"github.com/sapanjai/backend/internal/module/auth"
	"github.com/sapanjai/backend/internal/module/health"
	"github.com/sapanjai/backend/internal/module/organization"
	"github.com/sapanjai/backend/internal/module/rbac"
	"github.com/sapanjai/backend/internal/module/subscription"
	"github.com/sapanjai/backend/internal/shared/apperror"
	"github.com/sapanjai/backend/internal/shared/httpx"
	"github.com/sapanjai/backend/internal/shared/logger"
)

// New builds a fully configured Echo instance: middleware stack, custom
// error handler, infra-backed module wiring, and route registration.
func New(cfg *config.Config, log *slog.Logger, pool *pgxpool.Pool, rdb *redis.Client) *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

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
	redisAuth := appredis.NewAuth(rdb)
	tokenSvc := auth.NewTokenService(cfg)
	auditSvc := auditlog.NewService(store, log)

	authSvc := auth.NewService(store, redisAuth, auditSvc)
	authHandler := auth.NewHandler(authSvc, tokenSvc, store, redisAuth, cfg.JWTRefreshExpiresIn)
	authHandler.Register(e.Group("/auth"))

	rbacSvc := rbac.NewService(store)
	guards := appmw.NewGuards(tokenSvc, redisAuth, store, rbacSvc)
	subSvc := subscription.NewService(store)
	orgSvc := organization.NewService(store, auditSvc, subSvc)
	orgHandler := organization.NewHandler(orgSvc)
	orgHandler.Register(e.Group("/organizations"), guards)

	rbac.NewHandler(rbacSvc).Register(e.Group("/rbac"), guards)
	subHandler := subscription.NewHandler(subSvc)
	subHandler.Register(e.Group("/subscription"), guards)
	subHandler.RegisterPlans(e.Group("/plans"), guards)
	auditlog.NewHandler(auditSvc).Register(e.Group("/audit-logs"), guards)

	return e
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
