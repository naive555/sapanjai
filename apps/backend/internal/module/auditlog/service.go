// Package auditlog records best-effort audit trail entries and serves the
// audit-log query endpoint, mirroring AuditLogService in the source app
// (src/modules/audit-log/service.ts).
package auditlog

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sapanjai/backend/internal/infra/database/db"
)

// Recorded audit actions. Mirrors AuditAction in the source app.
// ActionOrgMemberRemoved, ActionRoleCreated, and ActionRoleAssigned are
// defined for parity but never written, matching the source and
// docs/02-api-contract.md ("defined but only the first four are currently
// written"). The three Connector* actions below them are outside that
// parity set — added with the connector module, and all three are written.
const (
	ActionUserLogin        = "user.login"
	ActionUserRegister     = "user.register"
	ActionOrgCreated       = "org.created"
	ActionOrgMemberInvited = "org.member.invited"
	ActionOrgMemberRemoved = "org.member.removed"
	ActionRoleCreated      = "role.created"
	ActionRoleAssigned     = "role.assigned"

	ActionConnectorCreated = "connector.created"
	ActionConnectorUpdated = "connector.updated"
	ActionConnectorDeleted = "connector.deleted"

	// MCP gateway actions (internal/module/mcp), written from the
	// tools/call-time enforcement middleware — see docs/07-sheets-adapter-decisions.md
	// step 3. All three are written; metadata never carries the bearer
	// token, decrypted connector config, or a whole request/tool-argument
	// struct (CLAUDE.md's redaction ground rule) — only connector_id, tool,
	// and (for a denial) missing_permission.
	ActionMCPSessionStarted = "mcp.session.started"
	ActionMCPToolCalled     = "mcp.tool.called"
	ActionMCPToolDenied     = "mcp.tool.denied"

	// ActionMCPRateLimitHit is written when a connector's rate-limit
	// bucket (internal/infra/redis.RateLimiter) is exhausted at
	// tools/call dispatch time — docs/07-sheets-adapter-decisions.md step 4.
	// Metadata carries connector_id and tool, the same shape as
	// ActionMCPToolDenied, minus missing_permission (this is a quota
	// failure, not an authorization one).
	ActionMCPRateLimitHit = "mcp.ratelimit.hit"

	// ActionMCPFileDownloaded is written by GET /mcp/files/:connectorId/
	// :fileId (internal/module/mcp/handler.go's downloadFile) once a
	// download has actually streamed — docs/07-sheets-adapter-decisions.md step
	// 9. This route has no bearer-token principal (the signed link itself
	// is the credential), so the actor recorded is the uid the link was
	// minted for, not a re-resolved live grant. Metadata carries
	// connector_id, file_id, and mime_type — never the file's own bytes,
	// the signature, or the connector's decrypted config.
	ActionMCPFileDownloaded = "mcp.file.downloaded"
)

// Service records audit log entries. Writes are best-effort: a failure is
// logged and swallowed, never propagated to the caller, per CLAUDE.md
// ("Audit-log writes are best-effort: log failures, never fail the
// request").
type Service struct {
	q   db.Querier
	log *slog.Logger
}

// NewService builds an auditlog Service. q is typically a *database.Store
// (outside a transaction) or a tx-bound *db.Queries (inside one) — both
// satisfy db.Querier.
func NewService(q db.Querier, log *slog.Logger) *Service {
	return &Service{q: q, log: log}
}

// Record inserts one audit row for action. userID and organizationID are
// optional (nil omits the column); metadata is an optional raw JSON blob.
// Errors are logged, not returned — this must never fail the caller's
// request.
func (s *Service) Record(ctx context.Context, action string, userID, organizationID *uuid.UUID, metadata []byte) {
	err := s.q.CreateAuditLog(ctx, db.CreateAuditLogParams{
		OrganizationID: toPgUUID(organizationID),
		UserID:         toPgUUID(userID),
		Action:         action,
		Metadata:       metadata,
	})
	if err != nil {
		s.log.Error("failed to record audit log", "error", err, "action", action)
	}
}

// Query returns organizationID's audit logs newest-first, optionally
// filtered by userID and/or action, capped at limit rows. Mirrors
// AuditLogService.query.
func (s *Service) Query(ctx context.Context, organizationID uuid.UUID, userID *uuid.UUID, action *string, limit int32) ([]db.AuditLog, error) {
	return s.q.QueryAuditLogs(ctx, db.QueryAuditLogsParams{
		OrganizationID: toPgUUID(&organizationID),
		UserID:         toPgUUID(userID),
		Action:         action,
		Lim:            limit,
	})
}

func toPgUUID(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *id, Valid: true}
}

// fromPgUUID is the inverse of toPgUUID, used when serializing a row read
// back from the database: an invalid (null) column becomes a nil pointer.
func fromPgUUID(id pgtype.UUID) *uuid.UUID {
	if !id.Valid {
		return nil
	}
	u := uuid.UUID(id.Bytes)
	return &u
}

// nonEmptyJSON coerces an empty/nil jsonb column to a nil json.RawMessage
// so it marshals as JSON null; json.Marshal panics on a non-nil empty
// []byte cast directly to json.RawMessage.
func nonEmptyJSON(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	return b
}
