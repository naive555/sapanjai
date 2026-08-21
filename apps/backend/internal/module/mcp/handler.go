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

// maxDownloadBytes caps how many bytes Handler.downloadFile will ever
// stream for one request — 25MiB, docs/07-sheets-adapter-plan.md step 9.
// Checked twice: once against the file's own reported SizeBytes before a
// single byte is streamed (a cheap, immediate 413), and again as a hard
// io.CopyN ceiling on the actual stream (in case Drive's reported size is
// stale or missing — the field is documented as present only for blobs) —
// the second check is what actually bounds memory/bandwidth; the first just
// fails fast and cheaply for the common case.
const maxDownloadBytes = 25 << 20

// connectorCtxKey is the *request context* key Handler.resolveConnector
// stores the resolved connector row under, read back by Handler.getServer.
// Same reasoning as appmw's principal key: the SDK's per-request server
// callback only ever sees r.Context(), never an echo.Context.
type connectorCtxKey struct{}

// Handler mounts POST /mcp/:connectorId: one Streamable HTTP MCP endpoint,
// shared by every connector, disambiguated per request by the path segment.
type Handler struct {
	service *Service
	mcpHTTP http.Handler
}

// NewHandler builds an mcp Handler, constructing the SDK's
// StreamableHTTPHandler once at server-boot time. Building it once (rather
// than per request) is required for Stateless mode — the handler itself is
// stateless and cheap to reuse; only the *mcp.Server built inside getServer
// is per-request.
func NewHandler(service *Service, log *slog.Logger) *Handler {
	h := &Handler{service: service}
	h.mcpHTTP = gomcp.NewStreamableHTTPHandler(h.getServer, &gomcp.StreamableHTTPOptions{
		// Stateless: no Mcp-Session-Id, no server-side session table, every
		// POST self-contained — auth, build the authorized surface, serve,
		// discard. This is what lets a permission or scope change take
		// effect on the next call instead of the next reconnect, and what
		// keeps the deployment horizontally scalable with no sticky
		// routing. See docs/05-mcp-gateway.md and
		// spikes/mcp-gateway/docs/FINDINGS.md §3.2.
		Stateless: true,
		// Plain application/json responses instead of SSE framing —
		// simpler to curl and to put behind an ordinary load balancer.
		JSONResponse: true,
		Logger:       log,
	})
	return h
}

// getServer is the SDK's per-request server-construction callback
// (func(*http.Request) *mcp.Server). It runs once per HTTP POST in
// Stateless mode, after Register's middleware chain (RequireMCPKey then
// resolveConnector) has already put a principal and a connector on the
// request's context. Both are guaranteed present by the time this runs; a
// missing one is a wiring bug (a route calling this without that chain
// ahead of it), and returning a server with no tools is the safe failure
// rather than a panic that would take the whole process down.
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

// baseURLFromRequest derives the scheme+host the gateway was reached on for
// r, for RequestInfo.BaseURL. Scheme prefers X-Forwarded-Proto (set by a
// reverse proxy/load balancer terminating TLS in front of this service,
// per the k8s deployment this runs behind) over r.TLS != nil, which would
// otherwise always read "http" behind a TLS-terminating proxy; host
// similarly prefers X-Forwarded-Host over r.Host. An empty r.Host (only
// happens in a hand-built *http.Request, i.e. a unit test that never went
// through a real net/http server) yields an empty BaseURL, which
// SignFileLink turns into a harmless root-relative "/mcp/files/..." URL —
// never served to a real client outside tests.
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

// Register mounts POST /mcp/:connectorId behind mcpKeyGuard (RequireMCPKey)
// and connector resolution, per docs/07-sheets-adapter-plan.md step 3:
// "Mount into Echo with echo.WrapHandler(mcpHandler)." Echo applies
// middleware in the order listed here — mcpKeyGuard runs first (resolves
// and authenticates the PAT), then resolveConnector (reads the principal it
// set, resolves :connectorId scoped to the principal's org), then finally
// the wrapped SDK handler.
func (h *Handler) Register(g *echo.Group, mcpKeyGuard echo.MiddlewareFunc) {
	g.POST("/:connectorId", echo.WrapHandler(h.mcpHTTP), mcpKeyGuard, h.resolveConnector)

	// GET /mcp/files/:connectorId/:fileId — the one gateway route
	// authenticated by URL signature rather than a bearer header
	// (docs/07-sheets-adapter-plan.md step 9). Deliberately *not* behind
	// mcpKeyGuard: the whole point of a signed, short-lived link
	// (drive_get_file, tools_drive.go) is that the agent's MCP client can
	// hand it to something else entirely (a browser, curl, a different
	// process) that has no PAT and no Authorization header at all — the
	// signature itself carries the authorization, verified in
	// Handler.downloadFile.
	//
	// This is unambiguous against POST /:connectorId despite both living
	// under the same "/mcp" group: Echo's router is a radix tree that tries
	// a literal path segment before falling back to a ":param" segment at
	// the same level, so a request for "/mcp/files/..." matches the
	// static "files" segment first and is never captured by ":connectorId"
	// — the same reason a REST API can mix "/users/me" and "/users/:id"
	// safely. Method also disambiguates: this route only ever answers GET,
	// the JSON-RPC route only POST.
	g.GET("/files/:connectorId/:fileId", h.downloadFile)
}

// resolveConnector is ordinary Echo middleware, not the SDK handler itself.
// It parses :connectorId and resolves it scoped to the authenticated
// principal's organization — never a client-supplied org — before a single
// byte of MCP protocol is processed. A connector belonging to another
// organization is apperror.NotFound, the same code a nonexistent id
// produces, so the id cannot be used to probe for another tenant's
// connectors.
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

// principalFromContext recovers the concrete *rbac.Principal that
// appmw.RequireMCPKey put on the request context. appmw.MCPPrincipalFromContext
// hands back `any` deliberately — the middleware package cannot import
// internal/module/rbac without an import cycle (see
// appmw.MCPPrincipalResolver's doc comment) — so this package, which safely
// imports rbac, is where the type assertion happens.
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

// downloadFile serves GET /mcp/files/:connectorId/:fileId — the signed
// download link drive_get_file (tools_drive.go) mints. Every failure short
// of the two explicitly-different-status cases below (413/429) is the exact
// same 404 apperror.NotFound: a bad signature, an expired link, an
// unresolvable connector, a wrong connector type, a file whose allowlisted
// folder was narrowed since the link was minted, a Google-native file with
// no bytes to stream, and a download that otherwise failed all collapse to
// one response a probing client cannot tell apart — mirroring the MCP
// gateway's own POST route, where a nonexistent connector and one belonging
// to another org are equally indistinguishable (Handler.resolveConnector).
//
// Order of operations (docs/07-sheets-adapter-plan.md step 9):
//  1. Parse every field the link carries and verify its signature/expiry.
//     This is the *only* authorization check on this route — there is no
//     bearer token, no session, nothing else standing between an
//     internet-reachable URL and a customer's file.
//  2. Resolve the connector, scoped to the org the link itself names (never
//     a value from anywhere else) — the signature is what makes that org
//     binding trustworthy, so no cross-tenant read is reachable this way.
//  3. Re-fetch the file's live metadata and allowlist status via the
//     adapter's GetFile — never the stale assumption "this file was
//     allowed when the link was minted." Narrowing scope.drive_folder_ids
//     kills every outstanding link for that file on its very next fetch,
//     the same "checked fresh on every call" discipline every sheets_* and
//     drive_* tool already follows.
//  4. A Google-native file (a Doc/Sheet/Slide) has no raw bytes at all.
//  5. Reject an oversized file before ever opening a stream.
//  6. Charge the connector's own rate-limit bucket for the download itself
//     — a second, separate upstream call from GetFile's own metadata fetch
//     above, so it earns its own charge (mirrors readRange's "one charge
//     per upstream call" discipline).
//  7. Stream via io.CopyN, capped at maxDownloadBytes and probing one byte
//     past it to detect (and log) a truncation — never buffer the whole
//     file in memory, the same discipline query.go's scan loop applies to
//     87GB of sheet rows.
//  8. Audit best-effort, after the stream finishes; never fail the response
//     on an audit error.
//  9. Nothing here ever logs the connector's config, the OAuth token, or
//     the link's own signature — only ids and a MIME type.
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
		return echo.NewHTTPError(http.StatusTooManyRequests, "Rate limit exceeded, try again later")
	}
	if !allowed {
		return echo.NewHTTPError(http.StatusTooManyRequests, rateLimitedMessage(retryAfter))
	}

	body, contentType, err := googlesheets.DownloadFile(ctx, ts, fileID)
	if err != nil {
		// Log this one, unlike the ChargeRateLimit/GetFile branches above
		// which return apperror.NotFound bare: this is exactly the kind of
		// failure that would have made the 15s-Client.Timeout bug (fixed in
		// client.go) invisible in production — a silent 404 with no trace
		// of *why* the download never started. connector_id and the error
		// only; never the token, the decrypted config, or the link's own
		// signature.
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

	// info.SizeBytes already rejected an oversized file above, but Drive
	// omits `size` for some entries (the field is documented as present
	// only for blobs and Workspace-editor exports), so that pre-check
	// cannot be trusted alone to bound what actually streams here. Reading
	// one byte past maxDownloadBytes — rather than exactly maxDownloadBytes
	// — is what lets this distinguish "the file was exactly the cap" from
	// "the file was larger and got cut off here": io.CopyN writes at most
	// maxDownloadBytes to resp either way (so the client-visible byte count
	// is never exceeded), and a copyErr of nil with a non-empty follow-up
	// read is the only way to tell the two cases apart. Headers/status are
	// already committed by this point, so a log line is the only honest
	// signal a truncation like this has — the response itself looks like a
	// clean 200 with a shorter file.
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

	// Best-effort, after the stream finishes, on a context detached from
	// the request's own cancellation — mirrors Service.enforce's
	// tools/call audit fix (service.go): a client that hangs up right as
	// the stream ends must not silently lose this row. Actor is the uid
	// the link was minted for (there is no bearer-token principal on this
	// route at all); org is the link's own org, already proven authentic
	// by VerifyFileLink above.
	h.service.recordAudit(context.WithoutCancel(ctx), auditlog.ActionMCPFileDownloaded,
		&rbac.Principal{UserID: uid, OrganizationID: orgID},
		map[string]any{
			"connector_id": conn.ID.String(),
			"file_id":      fileID,
			"mime_type":    info.MimeType,
		})

	return nil
}

// rateLimitedMessage matches RateLimited's (errors.go) wording for the
// REST-side 429s this route also produces, so a grep for "Retry after"
// finds every rate-limit denial in this module regardless of which
// transport it came back on.
func rateLimitedMessage(retryAfter time.Duration) string {
	return fmt.Sprintf("Rate limit exceeded. Retry after %d seconds.", int64(retryAfter.Round(time.Second).Seconds()))
}
