// Command worker runs background jobs: load config, connect to Redis and
// Postgres, start the job scheduler plus an internal /health server, and
// shut down gracefully on SIGINT/SIGTERM — mirroring cmd/api's bootstrap.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/sapanjai/backend/internal/config"
	"github.com/sapanjai/backend/internal/infra/database"
	"github.com/sapanjai/backend/internal/infra/redis"
	"github.com/sapanjai/backend/internal/job/emaildispatch"
	"github.com/sapanjai/backend/internal/job/sessioncleanup"
	"github.com/sapanjai/backend/internal/shared/email"
	applogger "github.com/sapanjai/backend/internal/shared/logger"
	"github.com/sapanjai/backend/internal/worker"
)

func main() {
	startedAt := time.Now()
	loadDotEnv()

	cfg, err := config.Load()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	log := applogger.New(cfg.AppEnv, cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rdb, err := redis.New(ctx, cfg.RedisURL)
	if err != nil {
		log.Error("failed to connect to redis", "error", err)
		os.Exit(1)
	}
	log.Info("Redis connected")

	pool, err := database.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	log.Info("Database connected")

	store := database.NewStore(pool)

	// The API only ever enqueues mail (email_outbox); this is the only
	// process that actually sends it, so this is the only place the choice
	// between a real provider and the log-only fallback is made. Never log
	// cfg.ResendAPIKey itself — pass it straight to the sender and nowhere
	// else.
	var sender email.Sender
	if cfg.EmailEnabled() {
		sender = email.NewResendSender(cfg.ResendAPIKey, cfg.EmailFrom)
		log.Info("email sender: resend", "from", cfg.EmailFrom)
	} else {
		sender = email.NewLogSender(log, cfg.AppEnv)
		log.Info("email sender: log (no RESEND_API_KEY configured)", "from", cfg.EmailFrom)
	}

	w := worker.New(worker.NewRedisLock(rdb, cfg.RedisKeyPrefix), log, cfg.WorkerJobTimeout)
	// Register future jobs here — one line each.
	w.Register(sessioncleanup.New(
		store, log,
		cfg.SessionCleanupInterval,
		cfg.SessionCleanupRetention,
		cfg.SessionCleanupBatchSize,
	))
	w.Register(emaildispatch.New(
		store, sender, log,
		cfg.EmailDispatchInterval,
		// The claim lease is sized off this, not off the interval: the worker
		// cancels a run at WORKER_JOB_TIMEOUT, so it bounds how long a batch
		// can still be in flight while another replica's run begins.
		cfg.WorkerJobTimeout,
		cfg.EmailDispatchBatchSize,
		cfg.EmailMaxAttempts,
		cfg.EmailOutboxRetention,
	))

	health := &http.Server{
		Addr:              ":" + cfg.WorkerPort,
		Handler:           worker.HealthHandler(w, startedAt),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Info("worker health server listening", "addr", health.Addr)
		if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("worker health server error", "error", err)
		}
	}()

	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	<-ctx.Done()
	log.Info("shutdown signal received, shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := health.Shutdown(shutdownCtx); err != nil {
		log.Error("error shutting down health server", "error", err)
	}

	select {
	case <-done:
	case <-shutdownCtx.Done():
		log.Warn("timed out waiting for in-flight jobs")
	}

	pool.Close()
	if err := rdb.Close(); err != nil {
		log.Error("error closing redis", "error", err)
	}

	log.Info("shutdown complete")
}

// loadDotEnv loads environment variables from a .env file if one is found,
// mirroring cmd/api's lookup so `go run ./cmd/worker` works the same way
// regardless of the working directory it's invoked from.
func loadDotEnv() {
	for _, path := range []string{"../../.env", "../.env", ".env"} {
		if err := godotenv.Load(path); err == nil {
			return
		}
	}
}
