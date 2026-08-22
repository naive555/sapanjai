package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sapanjai/backend/internal/adapter/googlesheets"
	"github.com/sapanjai/backend/internal/infra/database/db"
	appmw "github.com/sapanjai/backend/internal/middleware"
	"github.com/sapanjai/backend/internal/module/auditlog"
	"github.com/sapanjai/backend/internal/module/connector"
	"github.com/sapanjai/backend/internal/module/rbac"
	"github.com/sapanjai/backend/internal/shared/apperror"
)

// maxDownloadBytes caps one download at 25MiB. Checked twice: against the
// file's reported SizeBytes for a cheap early 413, then as a hard io.CopyN
// ceiling — the second is what actually bounds memory, since Drive reports
// size only for blobs and may report it stale.
const maxDownloadBytes = 25 << 20

// connectorCtxKey keys the resolved connector on the *request* context, not
// the echo.Context: the SDK's per-request server callback only sees
// r.Context().
type connectorCtxKey struct{}

// Handler mounts POST /mcp/:connectorId: one Streamable HTTP MCP endpoint,
// shared by every connector, disambiguated per request by the path segment.
type Handler struct {
	service *Service
	mcpHTTP http.Handler
}

// NewHandler builds an mcp Handler, constructing the SDK's
// StreamableHTTPHandler once at boot. The handler is stateless and reusable;
// only the *mcp.Server built inside getServer is per-request.
func NewHandler(service *Service, log *slog.Logger) *Handler {
	h := &Handler{service: service}
	h.mcpHTTP = gomcp.NewStreamableHTTPHandler(h.getServer, &gomcp.StreamableHTTPOptions{
		// Stateless: no session id, no server-side session table, every POST
		// self-contained. This is what makes a permission change take effect
		// on the next call rather than the next reconnect, and what keeps
		// the deployment scalable with no sticky routing.
		Stateless: true,
		// Plain application/json responses instead of SSE framing —
		// simpler to curl and to put behind an ordinary load balancer.
		JSONResponse: true,
		Logger:       log,
	})
	return h
}

// getServer is the SDK's per-request server-construction callback, run once
// per POST after Register's middleware chain has put a principal and a
// connector on the request context. A missing one is a wiring bug; returning
// a server with no tools fails safe rather than panicking the process.
func (h *Handler) getServer(r *http.Request) *gomcp.Server {
	req := RequestInfo{BaseURL: baseURLFromRequest(r)}
	p, ok := principalFromContext(r.Context())
	if !ok {
		return h.service.BuildServer(&rbac.Principal{}, db.Connector{}, req)
	}
	conn, ok := connectorFromContext(r.Context())
	if !ok {
		return h.service.BuildServer(&rbac.Principal{}, db.Connector{}, req)
	}
	return h.service.BuildServer(p, conn, req)
}

// baseURLFromRequest derives the scheme+host the gateway was reached on.
// X-Forwarded-* wins over r.TLS/r.Host, which would otherwise always read
// "http" behind the TLS-terminating proxy this runs behind. An empty r.Host
// (only in a hand-built request, i.e. a unit test) yields an empty BaseURL
// and a harmless root-relative link.
func baseURLFromRequest(r *http.Request) string {
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	if host == "" {
		return ""
	}
	scheme := r.Header.Get("X-Forwarded-Proto")
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	return scheme + "://" + host
}

// Register mounts the gateway routes. Echo applies middleware in the order
// listed: mcpKeyGuard authenticates the PAT, then resolveConnector resolves
// :connectorId scoped to that principal's org, then the SDK handler runs.
func (h *Handler) Register(g *echo.Group, mcpKeyGuard echo.MiddlewareFunc) {
	g.POST("/:connectorId", echo.WrapHandler(h.mcpHTTP), mcpKeyGuard, h.resolveConnector)

	// Deliberately not behind mcpKeyGuard: the point of a signed short-lived
	// link is that the client can hand it to a browser or curl that has no
	// PAT at all. The signature carries the authorization, checked in
	// downloadFile. Unambiguous against POST /:connectorId — Echo matches the
	// static "files" segment before the :param one, and the methods differ.
	g.GET("/files/:connectorId/:fileId", h.downloadFile)
}

// resolveConnector parses :connectorId and resolves it scoped to the
// authenticated principal's org — never a client-supplied one — before any
// MCP protocol is processed. Another org's connector yields the same
// NotFound a nonexistent id does, so ids cannot probe for other tenants.
func (h *Handler) resolveConnector(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		connectorID, err := connectorIDParam(c)
		if err != nil {
			return err
		}

		p, ok := principalFromContext(c.Request().Context())
		if !ok {
			// mcpKeyGuard must run ahead of this middleware; reaching here
			// without a principal is a wiring bug, not a client error.
			return apperror.New(apperror.NotFound)
		}

		conn, err := h.service.ResolveConnector(c.Request().Context(), p.OrganizationID, connectorID)
		if err != nil {
			return err
		}

		ctx := context.WithValue(c.Request().Context(), connectorCtxKey{}, conn)
		c.SetRequest(c.Request().WithContext(ctx))
		return next(c)
	}
}

func connectorFromContext(ctx context.Context) (db.Connector, bool) {
	conn, ok := ctx.Value(connectorCtxKey{}).(db.Connector)
	return conn, ok
}

// principalFromContext recovers the concrete *rbac.Principal RequireMCPKey
// put on the request context. That middleware hands back `any` because it
// cannot import rbac without a cycle, so the assertion happens here.
func principalFromContext(ctx context.Context) (*rbac.Principal, bool) {
	v, ok := appmw.MCPPrincipalFromContext(ctx)
	if !ok {
		return nil, false
	}
	p, ok := v.(*rbac.Principal)
	return p, ok
}

// connectorIDParam parses the :connectorId path param. A malformed id can
// never match a connector row, so it resolves to the same NotFound a
// well-formed-but-missing id would.
func connectorIDParam(c echo.Context) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Param("connectorId"))
	if err != nil {
		return uuid.Nil, apperror.New(apperror.NotFound)
	}
	return id, nil
}

// downloadFile serves the signed link drive_get_file mints.
//
// Every failure except 413 and 429 collapses to the same 404 — bad
// signature, expired link, unknown connector, wrong type, a folder
// de-allowlisted since minting, a Google-native file with no bytes — so a
// probing client can tell none of them apart, mirroring the POST route.
//
// Two things carry the security weight. The signature check is the *only*
// authorization on this route: there is no bearer token behind it. And the
// file's metadata and allowlist status are re-fetched live rather than
// trusted from mint time, so narrowing drive_folder_ids kills outstanding
// links on their next use. Nothing here logs config, the OAuth token, or the
// signature — only ids and a MIME type.
func (h *Handler) downloadFile(c echo.Context) error {
	ctx := c.Request().Context()

	connectorID, err := connectorIDParam(c)
	if err != nil {
		return apperror.New(apperror.NotFound)
	}
	fileID := c.Param("fileId") // Echo already URL-decodes path params.
	if fileID == "" {
		return apperror.New(apperror.NotFound)
	}
	orgID, err := uuid.Parse(c.QueryParam("org"))
	if err != nil {
		return apperror.New(apperror.NotFound)
	}
	uid, err := uuid.Parse(c.QueryParam("uid"))
	if err != nil {
		return apperror.New(apperror.NotFound)
	}
	exp, err := strconv.ParseInt(c.QueryParam("exp"), 10, 64)
	if err != nil {
		return apperror.New(apperror.NotFound)
	}
	sig := c.QueryParam("sig")

	if !VerifyFileLink(h.service.fileLinkKey, orgID, connectorID, uid, fileID, exp, sig, time.Now()) {
		// Covers every failure mode uniformly: no query at all (empty sig
		// fails hmac.Equal), a tampered field, an expired exp, and — since
		// VerifyFileLink fails closed on a zero-length key — link minting
		// having been disabled entirely (empty CONNECTOR_MASTER_KEY).
		return apperror.New(apperror.NotFound)
	}

	conn, err := h.service.connectors.Get(ctx, orgID, connectorID)
	if err != nil {
		return err
	}
	if conn.Type != string(connector.TypeGoogleSheets) {
		return apperror.New(apperror.NotFound)
	}

	cfg, err := h.service.openGoogleSheetsConfig(ctx, conn)
	if err != nil {
		return apperror.New(apperror.NotFound)
	}

	ts := h.service.sheetsTokens.Get(ctx, conn.ID, cfg.OAuth)
	info, err := googlesheets.GetFile(ctx, ts, cfg, conn.ID, h.service, fileID)
	if err != nil {
		var rlErr *googlesheets.RateLimitedError
		if errors.As(err, &rlErr) {
			return echo.NewHTTPError(http.StatusTooManyRequests, rateLimitedMessage(rlErr.RetryAfter))
		}
		// ErrFileNotAllowed (scope.drive_folder_ids narrowed since the link
		// was minted) and ErrFileNotFound (deleted, or never existed) both
		// collapse to the route's uniform 404.
		return apperror.New(apperror.NotFound)
	}

	if googlesheets.IsGoogleNativeMimeType(info.MimeType) {
		return apperror.New(apperror.NotFound)
	}
	if info.SizeBytes > maxDownloadBytes {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, "File too large")
	}

	allowed, retryAfter, err := h.service.ChargeRateLimit(ctx, conn.ID, 1)
	if err != nil {
		if h.service.log != nil {
			h.service.log.Error("mcp file download rate limiter check failed", "error", err, "connector_id", conn.ID)
		}
		return apperror.New(apperror.RateLimited)
	}
	if !allowed {
		return echo.NewHTTPError(http.StatusTooManyRequests, rateLimitedMessage(retryAfter))
	}

	body, contentType, err := googlesheets.DownloadFile(ctx, ts, fileID)
	if err != nil {
		// Logged, unlike the bare 404s above: a silent 404 here is what made
		// the Client.Timeout bug (fixed in client.go) invisible in
		// production. connector_id and the error only — never the token, the
		// config, or the signature.
		if h.service.log != nil {
			h.service.log.Error("mcp file download failed to start", "error", err, "connector_id", conn.ID)
		}
		return apperror.New(apperror.NotFound)
	}
	defer body.Close() //nolint:errcheck // best-effort close of an upstream response body

	resp := c.Response()
	resp.Header().Set(echo.HeaderContentType, contentType)
	resp.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename*=UTF-8''%s", url.PathEscape(info.Name)))
	resp.Header().Set("Cache-Control", "private, no-store")
	resp.Header().Set("X-Content-Type-Options", "nosniff")
	resp.WriteHeader(http.StatusOK)

	// Drive omits `size` for some entries, so the SizeBytes pre-check above
	// cannot alone bound what streams here. Probing one byte past the cap is
	// what separates "file was exactly the cap" from "file was truncated" —
	// io.CopyN never writes more than the cap either way. Headers are already
	// committed, so a log line is the only honest signal a truncation has.
	written, copyErr := io.CopyN(resp, body, maxDownloadBytes)
	switch {
	case copyErr == nil:
		var probe [1]byte
		if n, _ := body.Read(probe[:]); n > 0 && h.service.log != nil {
			h.service.log.Error("mcp file download truncated at maxDownloadBytes",
				"connector_id", conn.ID, "max_bytes", maxDownloadBytes, "bytes_copied", written)
		}
	case errors.Is(copyErr, io.EOF):
		// The file was shorter than the cap — expected, not an error.
	default:
		if h.service.log != nil {
			// A client that hung up mid-stream is the common case here,
			// not a bug — headers/status are already committed either way,
			// so there is nothing left to do but log.
			h.service.log.Error("mcp file download stream failed", "error", copyErr, "connector_id", conn.ID, "bytes_copied", written)
		}
	}

	// Detached from request cancellation so a client hanging up as the stream
	// ends doesn't lose the row. The actor is the uid the link was minted
	// for — there is no bearer principal on this route — and the org is the
	// link's own, already proven authentic by VerifyFileLink.
	h.service.recordAudit(context.WithoutCancel(ctx), auditlog.ActionMCPFileDownloaded,
		&rbac.Principal{UserID: uid, OrganizationID: orgID},
		map[string]any{
			"connector_id": conn.ID.String(),
			"file_id":      fileID,
			"mime_type":    info.MimeType,
		})

	return nil
}

// rateLimitedMessage matches RateLimited's wording (errors.go) so a grep for
// "Retry after" finds every denial in this module, whichever transport it
// came back on.
func rateLimitedMessage(retryAfter time.Duration) string {
	return fmt.Sprintf("Rate limit exceeded. Retry after %d seconds.", retrySeconds(retryAfter))
}
