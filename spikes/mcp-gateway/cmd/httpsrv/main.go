// Command httpsrv runs the spike as a Streamable HTTP MCP server — the shape
// the real Junctera gateway needs, because only an HTTP transport carries
// per-request headers and can therefore serve many tenants from one process.
//
// It runs Stateless, which makes the request lifecycle identical to a normal
// Echo route: authenticate the request, build the authorized surface, serve,
// discard. See docs/FINDINGS.md for why stateful mode is the wrong default
// here (the server instance is pinned at initialize time, so a permission
// change mid-session would not take effect).
//
//	go run ./cmd/httpsrv -addr :8090
//	curl -H 'Authorization: Bearer tok_reader_siam' ... http://127.0.0.1:8090/mcp
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/junctera/spikes/mcp-gateway/internal/gateway"
	"github.com/junctera/spikes/mcp-gateway/internal/principal"
	"github.com/junctera/spikes/mcp-gateway/internal/rbac"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8090", "listen address")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// getServer runs once per HTTP request in stateless mode. Auth has
	// already been enforced by withAuth, so the principal is guaranteed
	// present; a missing one is a programming error, and returning a server
	// with no tools is the safe failure.
	handler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		p, ok := principalFrom(r.Context())
		if !ok {
			logger.Error("no principal on authenticated request")
			return gateway.BuildServer(&rbac.Principal{}, logger)
		}
		return gateway.BuildServer(p, logger)
	}, &mcp.StreamableHTTPOptions{
		// Stateless: no Mcp-Session-Id, no server-side session table, every
		// POST self-contained. Horizontally scalable with no sticky routing.
		Stateless: true,
		// Plain application/json responses instead of SSE framing. Simpler
		// to curl and to put behind an ordinary load balancer; costs the
		// ability to stream partial results, which no tool here needs.
		JSONResponse: true,
		Logger:       logger,
	})

	mux := http.NewServeMux()
	mux.Handle("/mcp", withAuth(logger, handler))
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	logger.Info("junctera mcp spike listening",
		slog.String("addr", *addr),
		slog.String("endpoint", "http://"+*addr+"/mcp"),
		slog.Any("demo_tokens", principal.Tokens()),
	)

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("listen failed", slog.String("error", err.Error()))
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	logger.Info("shut down")
}

// ---------------------------------------------------------------------------
// auth
// ---------------------------------------------------------------------------

type ctxKey struct{}

// withAuth is the MCP analogue of controlplane's RequireAuth + RequireOrg,
// collapsed into one step because an MCP client has nowhere natural to put an
// x-organization-id header. The org is therefore bound to the credential
// rather than chosen per request — see docs/RBAC-MAPPING.md, "where the org
// comes from".
func withAuth(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			// WWW-Authenticate is what makes an MCP client offer to
			// re-authenticate instead of just erroring out.
			w.Header().Set("WWW-Authenticate", `Bearer realm="junctera"`)
			writeJSONError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		p, err := principal.Resolve(token)
		if err != nil {
			logger.Warn("auth rejected", slog.String("error", err.Error()))
			w.Header().Set("WWW-Authenticate", `Bearer realm="junctera", error="invalid_token"`)
			writeJSONError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	})
}

func principalFrom(ctx context.Context) (*rbac.Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(*rbac.Principal)
	return p, ok
}

func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return h[len(prefix):]
	}
	return ""
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
}
