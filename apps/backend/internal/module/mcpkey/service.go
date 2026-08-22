// Package mcpkey implements /mcp-keys: mint, list, and revoke org-scoped
// Personal Access Tokens that MCP clients present as a bearer credential.
// The credential only — no MCP protocol code lives here. Resolving a
// presented token to a caller, and intersecting its scopes with that
// caller's live RBAC grant, happens in internal/middleware.
package mcpkey

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sapanjai/backend/internal/infra/database"
	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/shared/apperror"
)

var _ mcpKeyStore = (*database.Store)(nil)

// tokenPrefix marks a token as a sapanjai-issued MCP PAT, mirroring the
// recognizable-prefix convention of Stripe/GitHub tokens so a leaked value
// is identifiable at a glance (e.g. in a secret-scanning tool).
const tokenPrefix = "sk_live_"

// tokenRandomBytes is the CSPRNG output backing each token: 256 bits.
const tokenRandomBytes = 32

// mcpKeyStore is the subset of *database.Store this service depends on,
// narrowed so unit tests can hand-mock it without the full db.Querier
// surface.
type mcpKeyStore interface {
	CreateMCPKey(ctx context.Context, arg db.CreateMCPKeyParams) (db.McpApiKey, error)
	GetMCPKeyByName(ctx context.Context, arg db.GetMCPKeyByNameParams) (db.McpApiKey, error)
	ListMCPKeysByOrg(ctx context.Context, organizationID uuid.UUID) ([]db.McpApiKey, error)
	RevokeMCPKey(ctx context.Context, arg db.RevokeMCPKeyParams) (int64, error)
}

// Service implements mint/list/revoke for MCP API keys.
type Service struct {
	store mcpKeyStore
	log   *slog.Logger
}

// NewService builds an mcpkey Service.
func NewService(store mcpKeyStore, log *slog.Logger) *Service {
	return &Service{store: store, log: log}
}

// Create mints a new key for organizationID, attributed to actorID. It
// returns the inserted row alongside the raw token — the caller's only
// opportunity to see it, since only its hash is ever persisted.
//
// Order: name collision, generate + hash, insert. expiresInDays, if given,
// sets an absolute expiry from now(); nil means the key never expires.
func (s *Service) Create(ctx context.Context, organizationID, actorID uuid.UUID, name string, expiresInDays *int) (db.McpApiKey, string, error) {
	_, err := s.store.GetMCPKeyByName(ctx, db.GetMCPKeyByNameParams{OrganizationID: organizationID, Name: name})
	if err == nil {
		return db.McpApiKey{}, "", apperror.New(apperror.MCPKeyNameTaken)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return db.McpApiKey{}, "", err
	}

	rawToken, err := generateToken()
	if err != nil {
		return db.McpApiKey{}, "", fmt.Errorf("generate token: %w", err)
	}

	var expiresAt pgtype.Timestamp
	if expiresInDays != nil {
		expiresAt = pgtype.Timestamp{Time: time.Now().Add(time.Duration(*expiresInDays) * 24 * time.Hour), Valid: true}
	}

	row, err := s.store.CreateMCPKey(ctx, db.CreateMCPKeyParams{
		OrganizationID: organizationID,
		UserID:         actorID,
		Name:           name,
		KeyHash:        HashToken(rawToken),
		ExpiresAt:      expiresAt,
	})
	if err != nil {
		return db.McpApiKey{}, "", err
	}

	return row, rawToken, nil
}

// List returns organizationID's keys, oldest first. Rows carry KeyHash —
// callers (the handler) must not map it onto any response DTO.
func (s *Service) List(ctx context.Context, organizationID uuid.UUID) ([]db.McpApiKey, error) {
	return s.store.ListMCPKeysByOrg(ctx, organizationID)
}

// Revoke sets revoked_at on keyID scoped to organizationID. A key belonging
// to another org is indistinguishable from one that does not exist: both
// are NOT_FOUND, so an id cannot be probed for existence across tenants.
// Revoking an already-revoked key is idempotent — it re-stamps revoked_at
// and still succeeds, rather than surfacing a distinct error.
func (s *Service) Revoke(ctx context.Context, organizationID, keyID uuid.UUID) error {
	rows, err := s.store.RevokeMCPKey(ctx, db.RevokeMCPKeyParams{ID: keyID, OrganizationID: organizationID})
	if err != nil {
		return err
	}
	if rows == 0 {
		return apperror.New(apperror.MCPKeyNotFound)
	}
	return nil
}

// generateToken returns a fresh "sk_live_<base64url(32 random bytes)>"
// token, per Decision 1.
func generateToken() (string, error) {
	buf := make([]byte, tokenRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashToken returns the hex-encoded SHA-256 digest of a raw token. Exported
// so internal/middleware.RequireMCPKey can hash a presented bearer token the
// same way before its GetMCPKeyByHash lookup — one hashing implementation,
// two callers (mint-time here, verify-time there).
//
// Deliberate departure from CLAUDE.md's bcrypt-cost-12 rule: that rule
// exists for *passwords* — low-entropy secrets a human chose, where the
// point of a slow hash is to make offline brute force expensive. A PAT is
// 256 bits of crypto/rand output, so brute force is already infeasible, and
// bcrypt is both too slow to run on every MCP call and — critically —
// impossible to index for a lookup-by-token query (bcrypt salts each hash
// differently, so equal inputs don't produce equal outputs). SHA-256 is
// deterministic, so key_hash carries a unique index and revocation/lookup
// is a single indexed read. Do not "fix" this to bcrypt.
func HashToken(rawToken string) string {
	sum := sha256.Sum256([]byte(rawToken))
	return hex.EncodeToString(sum[:])
}
