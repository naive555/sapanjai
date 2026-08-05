package worker

import (
	"encoding/json"
	"net/http"
	"time"
)

// HealthHandler serves GET /health for the worker process: the same
// {status, uptime} shape as the API's health module, plus per-job stats.
// It is an internal ops endpoint (WORKER_PORT), not part of the public API
// contract in docs/02-api-contract.md.
func HealthHandler(w *Worker, startedAt time.Time) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"status": "ok",
			"uptime": time.Since(startedAt).Seconds(),
			"jobs":   w.Stats(),
		})
	})
	return mux
}
