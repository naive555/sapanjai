// Package connector implements the /connectors module: org-scoped CRUD over
// upstream connections whose credentials are sealed at rest with envelope
// encryption (internal/shared/envelope), plus a health-check stub.
//
// The one invariant worth stating twice: decrypted config never leaves this
// package. It is sealed on the way in, opened only to hand to a Checker, and
// no response DTO or log line carries it.
package connector

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sapanjai/backend/internal/infra/database"
	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/module/auditlog"
	"github.com/sapanjai/backend/internal/module/subscription"
	"github.com/sapanjai/backend/internal/shared/apperror"
	"github.com/sapanjai/backend/internal/shared/envelope"
)

var (
	_ connectorStore = (*database.Store)(nil)
	_ limitEnforcer  = (*subscription.Service)(nil)
	_ sealer         = (*envelope.Encryptor)(nil)
)

// maxConnectorsLimitKey is the plans.limits key gating connector creation,
// mirroring max_members for org invites. An org whose plan omits it is
// unlimited (subscription.Service.EnforceLimit).
const maxConnectorsLimitKey = "max_connectors"

// connectorStore is the subset of *database.Store this service depends on,
// narrowed so unit tests can hand-mock it without the full db.Querier
// surface.
type connectorStore interface {
	CreateConnector(ctx context.Context, arg db.CreateConnectorParams) (db.Connector, error)
	GetConnector(ctx context.Context, arg db.GetConnectorParams) (db.Connector, error)
	GetConnectorByName(ctx context.Context, arg db.GetConnectorByNameParams) (db.Connector, error)
	ListConnectorsByOrg(ctx context.Context, organizationID uuid.UUID) ([]db.Connector, error)
	CountConnectorsByOrg(ctx context.Context, organizationID uuid.UUID) (int64, error)
	UpdateConnector(ctx context.Context, arg db.UpdateConnectorParams) (db.Connector, error)
	UpdateConnectorHealth(ctx context.Context, arg db.UpdateConnectorHealthParams) (db.Connector, error)
	DeleteConnector(ctx context.Context, arg db.DeleteConnectorParams) (int64, error)
}

// sealer is the subset of *envelope.Encryptor this service depends on,
// narrowed so tests can substitute a fake and so the KMS swap stays a
// constructor-level change.
type sealer interface {
	Seal(ctx context.Context, plaintext, aad []byte) (json.RawMessage, error)
	Open(ctx context.Context, raw json.RawMessage, aad []byte) ([]byte, error)
}

// limitEnforcer is the subset of *subscription.Service Create depends on.
type limitEnforcer interface {
	EnforceLimit(ctx context.Context, organizationID uuid.UUID, key string, currentCount int) error
}

// Service implements connector CRUD and the health-check entry point.
type Service struct {
	store    connectorStore
	crypto   sealer
	audit    *auditlog.Service
	limits   limitEnforcer
	checkers Registry
	log      *slog.Logger
}

// NewService builds a connector Service. checkers is empty today; see
// health.go.
func NewService(store connectorStore, crypto sealer, audit *auditlog.Service, limits limitEnforcer, checkers Registry, log *slog.Logger) *Service {
	return &Service{store: store, crypto: crypto, audit: audit, limits: limits, checkers: checkers, log: log}
}

// Create seals config and inserts a connector, in this order: type validity,
// plan limit, name collision, seal, insert. New connectors start "inactive"
// (the column default) — nothing has probed the upstream yet.
func (s *Service) Create(ctx context.Context, organizationID, actorID uuid.UUID, name, typ string, config map[string]any) (db.Connector, error) {
	if !IsValidType(typ) {
		return db.Connector{}, apperror.New(apperror.InvalidConnectorType)
	}

	count, err := s.store.CountConnectorsByOrg(ctx, organizationID)
	if err != nil {
		return db.Connector{}, err
	}
	if err := s.limits.EnforceLimit(ctx, organizationID, maxConnectorsLimitKey, int(count)); err != nil {
		return db.Connector{}, err
	}

	_, err = s.store.GetConnectorByName(ctx, db.GetConnectorByNameParams{OrganizationID: organizationID, Name: name})
	if err == nil {
		return db.Connector{}, apperror.New(apperror.ConnectorNameTaken)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return db.Connector{}, err
	}

	sealedConfig, err := s.sealConfig(ctx, organizationID, config)
	if err != nil {
		return db.Connector{}, err
	}

	row, err := s.store.CreateConnector(ctx, db.CreateConnectorParams{
		OrganizationID:  organizationID,
		Name:            name,
		Type:            typ,
		EncryptedConfig: sealedConfig,
	})
	if err != nil {
		return db.Connector{}, err
	}

	metadata, _ := json.Marshal(map[string]string{"name": name, "type": typ})
	s.audit.Record(ctx, auditlog.ActionConnectorCreated, &actorID, &organizationID, metadata)

	return row, nil
}

// List returns organizationID's connectors, oldest first.
func (s *Service) List(ctx context.Context, organizationID uuid.UUID) ([]db.Connector, error) {
	return s.store.ListConnectorsByOrg(ctx, organizationID)
}

// Get returns one connector scoped to organizationID. A connector belonging
// to another org is indistinguishable from one that does not exist: both are
// NOT_FOUND, so an id cannot be probed for existence across tenants.
func (s *Service) Get(ctx context.Context, organizationID, connectorID uuid.UUID) (db.Connector, error) {
	return s.get(ctx, organizationID, connectorID)
}

// Update applies the non-nil fields of a patch. It reads the row first (which
// also authorizes it against organizationID), merges in Go, and issues one
// write — see plan decision 7. A supplied config is sealed under a brand-new
// data key; the previous ciphertext is overwritten, not versioned.
func (s *Service) Update(ctx context.Context, organizationID, actorID, connectorID uuid.UUID, name, status *string, config map[string]any) (db.Connector, error) {
	existing, err := s.get(ctx, organizationID, connectorID)
	if err != nil {
		return db.Connector{}, err
	}

	newName := existing.Name
	if name != nil {
		newName = *name
	}

	newStatus := existing.Status
	if status != nil {
		newStatus = *status
	}

	newConfig := existing.EncryptedConfig
	configRotated := config != nil
	if configRotated {
		sealedConfig, err := s.sealConfig(ctx, organizationID, config)
		if err != nil {
			return db.Connector{}, err
		}
		newConfig = sealedConfig
	}

	row, err := s.store.UpdateConnector(ctx, db.UpdateConnectorParams{
		ID:              connectorID,
		OrganizationID:  organizationID,
		Name:            newName,
		Status:          newStatus,
		EncryptedConfig: newConfig,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.Connector{}, apperror.New(apperror.NotFound)
		}
		return db.Connector{}, err
	}

	metadata := map[string]string{"name": newName}
	if configRotated {
		metadata["config_rotated"] = "true"
	}
	metaBytes, _ := json.Marshal(metadata)
	s.audit.Record(ctx, auditlog.ActionConnectorUpdated, &actorID, &organizationID, metaBytes)

	return row, nil
}

// Delete removes a connector scoped to organizationID.
func (s *Service) Delete(ctx context.Context, organizationID, actorID, connectorID uuid.UUID) error {
	rows, err := s.store.DeleteConnector(ctx, db.DeleteConnectorParams{ID: connectorID, OrganizationID: organizationID})
	if err != nil {
		return err
	}
	if rows == 0 {
		return apperror.New(apperror.NotFound)
	}

	metadata, _ := json.Marshal(map[string]string{"connector_id": connectorID.String()})
	s.audit.Record(ctx, auditlog.ActionConnectorDeleted, &actorID, &organizationID, metadata)

	return nil
}

// CheckHealth probes connectorID's upstream and records the outcome on the
// row. The skeleton registers no Checkers, so every type returns
// HEALTH_CHECK_UNSUPPORTED today and the row is left untouched — the decrypt
// path below only starts executing once a real adapter lands.
func (s *Service) CheckHealth(ctx context.Context, organizationID, connectorID uuid.UUID) (db.Connector, error) {
	row, err := s.get(ctx, organizationID, connectorID)
	if err != nil {
		return db.Connector{}, err
	}

	checker, ok := s.checkers[Type(row.Type)]
	if !ok {
		return db.Connector{}, apperror.New(apperror.HealthCheckUnsupported)
	}

	config, err := s.openConfig(ctx, organizationID, row.EncryptedConfig)
	if err != nil {
		s.log.Error("failed to open connector config", "connector_id", row.ID, "error", err)
		return db.Connector{}, err
	}

	status := StatusActive
	if err := checker.Check(ctx, config); err != nil {
		s.log.Warn("connector health check failed", "connector_id", row.ID, "type", row.Type, "error", err)
		status = StatusError
	}

	updated, err := s.store.UpdateConnectorHealth(ctx, db.UpdateConnectorHealthParams{
		ID:             connectorID,
		OrganizationID: organizationID,
		Status:         string(status),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.Connector{}, apperror.New(apperror.NotFound)
		}
		return db.Connector{}, err
	}

	return updated, nil
}

// OpenConfig decrypts an already-fetched connector row's config, scoped to
// organizationID exactly as sealConfig bound it. Exported for a caller that
// already holds a connector row (via Get) and needs the decrypted config
// in-process to build an upstream client itself — the MCP gateway
// (internal/module/mcp), whose tool handlers need a live Google client, not
// just a health-check probe result. Same invariant as CheckHealth's own use
// of openConfig: the returned map is this call's only copy and must never
// reach a DTO, a log line, or an audit field. Re-decrypting on every call
// (rather than caching the result) is deliberate — see
// docs/07-sheets-adapter-decisions.md step 5's "every tool calls [the allowlist]
// on every request against the stored config, never against a value cached
// from connector-creation time": a caller that re-fetches the row per
// request and calls this each time automatically re-reads a narrowed
// allowlist on the very next call.
func (s *Service) OpenConfig(ctx context.Context, organizationID uuid.UUID, encryptedConfig json.RawMessage) (map[string]any, error) {
	return s.openConfig(ctx, organizationID, encryptedConfig)
}

// get fetches connectorID scoped to organizationID, translating a missing
// row into apperror.NotFound. Shared by Get/Update/Delete/CheckHealth.
func (s *Service) get(ctx context.Context, organizationID, connectorID uuid.UUID) (db.Connector, error) {
	row, err := s.store.GetConnector(ctx, db.GetConnectorParams{ID: connectorID, OrganizationID: organizationID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.Connector{}, apperror.New(apperror.NotFound)
		}
		return db.Connector{}, err
	}
	return row, nil
}

// sealConfig marshals and seals a plaintext config for organizationID.
func (s *Service) sealConfig(ctx context.Context, organizationID uuid.UUID, config map[string]any) (json.RawMessage, error) {
	plaintext, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	return s.crypto.Seal(ctx, plaintext, aad(organizationID))
}

// openConfig reverses sealConfig.
func (s *Service) openConfig(ctx context.Context, organizationID uuid.UUID, raw json.RawMessage) (map[string]any, error) {
	plaintext, err := s.crypto.Open(ctx, raw, aad(organizationID))
	if err != nil {
		return nil, err
	}
	var config map[string]any
	if err := json.Unmarshal(plaintext, &config); err != nil {
		return nil, err
	}
	return config, nil
}

// aad binds a connector's ciphertext to its owning organization: a row moved
// to another tenant fails to open instead of decrypting. Defence in depth —
// every query is already scoped by organization_id.
func aad(organizationID uuid.UUID) []byte {
	return organizationID[:]
}
