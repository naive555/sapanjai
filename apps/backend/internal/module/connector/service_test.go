package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/module/auditlog"
	"github.com/sapanjai/backend/internal/shared/apperror"
	"github.com/sapanjai/backend/internal/shared/envelope"
)

// ---- hand-mocked connectorStore ----

type mockConnectorStore struct {
	createConnector       func(ctx context.Context, arg db.CreateConnectorParams) (db.Connector, error)
	getConnector          func(ctx context.Context, arg db.GetConnectorParams) (db.Connector, error)
	getConnectorByName    func(ctx context.Context, arg db.GetConnectorByNameParams) (db.Connector, error)
	listConnectorsByOrg   func(ctx context.Context, organizationID uuid.UUID) ([]db.Connector, error)
	countConnectorsByOrg  func(ctx context.Context, organizationID uuid.UUID) (int64, error)
	updateConnector       func(ctx context.Context, arg db.UpdateConnectorParams) (db.Connector, error)
	updateConnectorHealth func(ctx context.Context, arg db.UpdateConnectorHealthParams) (db.Connector, error)
	deleteConnector       func(ctx context.Context, arg db.DeleteConnectorParams) (int64, error)
}

func (m *mockConnectorStore) CreateConnector(ctx context.Context, arg db.CreateConnectorParams) (db.Connector, error) {
	return m.createConnector(ctx, arg)
}
func (m *mockConnectorStore) GetConnector(ctx context.Context, arg db.GetConnectorParams) (db.Connector, error) {
	return m.getConnector(ctx, arg)
}
func (m *mockConnectorStore) GetConnectorByName(ctx context.Context, arg db.GetConnectorByNameParams) (db.Connector, error) {
	return m.getConnectorByName(ctx, arg)
}
func (m *mockConnectorStore) ListConnectorsByOrg(ctx context.Context, organizationID uuid.UUID) ([]db.Connector, error) {
	return m.listConnectorsByOrg(ctx, organizationID)
}
func (m *mockConnectorStore) CountConnectorsByOrg(ctx context.Context, organizationID uuid.UUID) (int64, error) {
	return m.countConnectorsByOrg(ctx, organizationID)
}
func (m *mockConnectorStore) UpdateConnector(ctx context.Context, arg db.UpdateConnectorParams) (db.Connector, error) {
	return m.updateConnector(ctx, arg)
}
func (m *mockConnectorStore) UpdateConnectorHealth(ctx context.Context, arg db.UpdateConnectorHealthParams) (db.Connector, error) {
	return m.updateConnectorHealth(ctx, arg)
}
func (m *mockConnectorStore) DeleteConnector(ctx context.Context, arg db.DeleteConnectorParams) (int64, error) {
	return m.deleteConnector(ctx, arg)
}

var _ connectorStore = (*mockConnectorStore)(nil)

// ---- hand-mocked limitEnforcer ----

type mockLimitEnforcer struct {
	enforceLimit func(ctx context.Context, organizationID uuid.UUID, key string, currentCount int) error
}

func (m *mockLimitEnforcer) EnforceLimit(ctx context.Context, organizationID uuid.UUID, key string, currentCount int) error {
	return m.enforceLimit(ctx, organizationID, key, currentCount)
}

var _ limitEnforcer = (*mockLimitEnforcer)(nil)

func allowAllLimiter() *mockLimitEnforcer {
	return &mockLimitEnforcer{
		enforceLimit: func(ctx context.Context, organizationID uuid.UUID, key string, currentCount int) error {
			return nil
		},
	}
}

// ---- failingSealer: a sealer whose Open (and Seal) always fail ----

type failingSealer struct{ err error }

func (f *failingSealer) Seal(ctx context.Context, plaintext, aad []byte) (json.RawMessage, error) {
	return nil, f.err
}
func (f *failingSealer) Open(ctx context.Context, raw json.RawMessage, aad []byte) ([]byte, error) {
	return nil, f.err
}

var _ sealer = (*failingSealer)(nil)

// ---- fakeChecker: a Checker that records the config it was given ----

type fakeChecker struct {
	typ       Type
	err       error
	gotConfig map[string]any
}

func (f *fakeChecker) Type() Type { return f.typ }
func (f *fakeChecker) Check(ctx context.Context, config map[string]any) error {
	f.gotConfig = config
	return f.err
}

var _ Checker = (*fakeChecker)(nil)

// ---- spyQuerier: records CreateAuditLog calls for assertion ----

type spyQuerier struct {
	db.Querier
	auditCalls []db.CreateAuditLogParams
}

func (s *spyQuerier) CreateAuditLog(ctx context.Context, arg db.CreateAuditLogParams) error {
	s.auditCalls = append(s.auditCalls, arg)
	return nil
}

func newTestAudit(spy *spyQuerier) *auditlog.Service {
	return auditlog.NewService(spy, slog.New(slog.NewTextHandler(os.Stdout, nil)))
}

func newTestCrypto(t *testing.T) *envelope.Encryptor {
	t.Helper()
	provider, err := envelope.NewEnvKeyProvider(bytes.Repeat([]byte{1}, envelope.MasterKeyLen))
	if err != nil {
		t.Fatalf("NewEnvKeyProvider: %v", err)
	}
	return envelope.New(provider)
}

func newTestLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, nil))
}

func appErrorCode(t *testing.T, err error) string {
	t.Helper()
	var appErr *apperror.Error
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *apperror.Error, got %T: %v", err, err)
	}
	return appErr.Code
}

// ---- Create ----

func TestService_Create_HappyPath(t *testing.T) {
	orgID := uuid.New()
	actorID := uuid.New()
	crypto := newTestCrypto(t)
	spy := &spyQuerier{}
	audit := newTestAudit(spy)

	var captured db.CreateConnectorParams
	store := &mockConnectorStore{
		countConnectorsByOrg: func(ctx context.Context, organizationID uuid.UUID) (int64, error) {
			return 0, nil
		},
		getConnectorByName: func(ctx context.Context, arg db.GetConnectorByNameParams) (db.Connector, error) {
			return db.Connector{}, pgx.ErrNoRows
		},
		createConnector: func(ctx context.Context, arg db.CreateConnectorParams) (db.Connector, error) {
			captured = arg
			return db.Connector{
				ID:              uuid.New(),
				OrganizationID:  arg.OrganizationID,
				Name:            arg.Name,
				Type:            arg.Type,
				EncryptedConfig: arg.EncryptedConfig,
				Status:          "inactive",
			}, nil
		},
	}

	svc := NewService(store, crypto, audit, allowAllLimiter(), NewRegistry(), newTestLog())

	config := map[string]any{"host": "db.example.com", "password": "hunter2-secret"}
	row, err := svc.Create(context.Background(), orgID, actorID, "warehouse-db", string(TypeGeneric), config)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if row.Name != "warehouse-db" || row.Type != string(TypeGeneric) {
		t.Fatalf("row = %+v, want name=warehouse-db type=%s", row, TypeGeneric)
	}

	if captured.OrganizationID != orgID || captured.Name != "warehouse-db" || captured.Type != string(TypeGeneric) {
		t.Fatalf("captured params = %+v, want org=%s name=warehouse-db type=%s", captured, orgID, TypeGeneric)
	}
	if bytes.Contains(captured.EncryptedConfig, []byte("hunter2-secret")) {
		t.Fatalf("stored config leaks the plaintext secret: %s", captured.EncryptedConfig)
	}

	opened, err := crypto.Open(context.Background(), captured.EncryptedConfig, aad(orgID))
	if err != nil {
		t.Fatalf("Open stored config: %v", err)
	}
	var gotConfig map[string]any
	if err := json.Unmarshal(opened, &gotConfig); err != nil {
		t.Fatalf("unmarshal opened config: %v", err)
	}
	if gotConfig["password"] != "hunter2-secret" || gotConfig["host"] != "db.example.com" {
		t.Fatalf("opened config = %v, want %v", gotConfig, config)
	}

	if len(spy.auditCalls) != 1 {
		t.Fatalf("audit calls = %d, want 1", len(spy.auditCalls))
	}
	if spy.auditCalls[0].Action != auditlog.ActionConnectorCreated {
		t.Errorf("audit action = %q, want %q", spy.auditCalls[0].Action, auditlog.ActionConnectorCreated)
	}
	if bytes.Contains(spy.auditCalls[0].Metadata, []byte("hunter2-secret")) {
		t.Fatalf("audit metadata leaks the secret: %s", spy.auditCalls[0].Metadata)
	}
	var meta map[string]string
	if err := json.Unmarshal(spy.auditCalls[0].Metadata, &meta); err != nil {
		t.Fatalf("unmarshal audit metadata: %v", err)
	}
	if meta["name"] != "warehouse-db" || meta["type"] != string(TypeGeneric) {
		t.Fatalf("audit metadata = %v, want name/type set", meta)
	}
}

func TestService_Create_InvalidType(t *testing.T) {
	svc := NewService(&mockConnectorStore{}, newTestCrypto(t), newTestAudit(&spyQuerier{}), allowAllLimiter(), NewRegistry(), newTestLog())

	_, err := svc.Create(context.Background(), uuid.New(), uuid.New(), "x", "not-a-real-type", map[string]any{"a": 1})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if code := appErrorCode(t, err); code != apperror.InvalidConnectorType {
		t.Fatalf("code = %q, want %q", code, apperror.InvalidConnectorType)
	}
}

func TestService_Create_LimitExceeded(t *testing.T) {
	store := &mockConnectorStore{
		countConnectorsByOrg: func(ctx context.Context, organizationID uuid.UUID) (int64, error) {
			return 5, nil
		},
	}
	limits := &mockLimitEnforcer{
		enforceLimit: func(ctx context.Context, organizationID uuid.UUID, key string, currentCount int) error {
			return apperror.New(apperror.LimitExceeded)
		},
	}
	svc := NewService(store, newTestCrypto(t), newTestAudit(&spyQuerier{}), limits, NewRegistry(), newTestLog())

	_, err := svc.Create(context.Background(), uuid.New(), uuid.New(), "x", string(TypeGeneric), map[string]any{"a": 1})
	if code := appErrorCode(t, err); code != apperror.LimitExceeded {
		t.Fatalf("code = %q, want %q", code, apperror.LimitExceeded)
	}
}

func TestService_Create_NameTaken(t *testing.T) {
	store := &mockConnectorStore{
		countConnectorsByOrg: func(ctx context.Context, organizationID uuid.UUID) (int64, error) {
			return 0, nil
		},
		getConnectorByName: func(ctx context.Context, arg db.GetConnectorByNameParams) (db.Connector, error) {
			return db.Connector{ID: uuid.New()}, nil
		},
	}
	svc := NewService(store, newTestCrypto(t), newTestAudit(&spyQuerier{}), allowAllLimiter(), NewRegistry(), newTestLog())

	_, err := svc.Create(context.Background(), uuid.New(), uuid.New(), "dup-name", string(TypeGeneric), map[string]any{"a": 1})
	if code := appErrorCode(t, err); code != apperror.ConnectorNameTaken {
		t.Fatalf("code = %q, want %q", code, apperror.ConnectorNameTaken)
	}
}

func TestService_Create_NameLookupErrorPropagates(t *testing.T) {
	wantErr := errors.New("boom: db unavailable")
	store := &mockConnectorStore{
		countConnectorsByOrg: func(ctx context.Context, organizationID uuid.UUID) (int64, error) {
			return 0, nil
		},
		getConnectorByName: func(ctx context.Context, arg db.GetConnectorByNameParams) (db.Connector, error) {
			return db.Connector{}, wantErr
		},
	}
	svc := NewService(store, newTestCrypto(t), newTestAudit(&spyQuerier{}), allowAllLimiter(), NewRegistry(), newTestLog())

	_, err := svc.Create(context.Background(), uuid.New(), uuid.New(), "x", string(TypeGeneric), map[string]any{"a": 1})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	var appErr *apperror.Error
	if errors.As(err, &appErr) {
		t.Fatalf("expected a plain error, got *apperror.Error(%s)", appErr.Code)
	}
}

// ---- Get ----

func TestService_Get_NotFoundForOtherOrgOrMissingID(t *testing.T) {
	store := &mockConnectorStore{
		getConnector: func(ctx context.Context, arg db.GetConnectorParams) (db.Connector, error) {
			return db.Connector{}, pgx.ErrNoRows
		},
	}
	svc := NewService(store, newTestCrypto(t), newTestAudit(&spyQuerier{}), allowAllLimiter(), NewRegistry(), newTestLog())

	_, err := svc.Get(context.Background(), uuid.New(), uuid.New())
	if code := appErrorCode(t, err); code != apperror.NotFound {
		t.Fatalf("code = %q, want %q", code, apperror.NotFound)
	}
}

func TestService_Get_ScopesLookupToCallerOrg(t *testing.T) {
	orgID := uuid.New()
	connID := uuid.New()
	var captured db.GetConnectorParams
	store := &mockConnectorStore{
		getConnector: func(ctx context.Context, arg db.GetConnectorParams) (db.Connector, error) {
			captured = arg
			return db.Connector{ID: connID, OrganizationID: orgID}, nil
		},
	}
	svc := NewService(store, newTestCrypto(t), newTestAudit(&spyQuerier{}), allowAllLimiter(), NewRegistry(), newTestLog())

	if _, err := svc.Get(context.Background(), orgID, connID); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if captured.ID != connID || captured.OrganizationID != orgID {
		t.Fatalf("captured params = %+v, want id=%s org=%s", captured, connID, orgID)
	}
}

// ---- List ----

func TestService_List_ScopesToOrg(t *testing.T) {
	orgID := uuid.New()
	var captured uuid.UUID
	store := &mockConnectorStore{
		listConnectorsByOrg: func(ctx context.Context, organizationID uuid.UUID) ([]db.Connector, error) {
			captured = organizationID
			return nil, nil
		},
	}
	svc := NewService(store, newTestCrypto(t), newTestAudit(&spyQuerier{}), allowAllLimiter(), NewRegistry(), newTestLog())

	if _, err := svc.List(context.Background(), orgID); err != nil {
		t.Fatalf("List: %v", err)
	}
	if captured != orgID {
		t.Fatalf("captured org = %s, want %s", captured, orgID)
	}
}

// ---- Update ----

func TestService_Update_NotFound(t *testing.T) {
	store := &mockConnectorStore{
		getConnector: func(ctx context.Context, arg db.GetConnectorParams) (db.Connector, error) {
			return db.Connector{}, pgx.ErrNoRows
		},
	}
	svc := NewService(store, newTestCrypto(t), newTestAudit(&spyQuerier{}), allowAllLimiter(), NewRegistry(), newTestLog())

	newName := "new-name"
	_, err := svc.Update(context.Background(), uuid.New(), uuid.New(), uuid.New(), &newName, nil, nil)
	if code := appErrorCode(t, err); code != apperror.NotFound {
		t.Fatalf("code = %q, want %q", code, apperror.NotFound)
	}
}

func TestService_Update_NameOnly_PreservesStatusAndConfig(t *testing.T) {
	orgID := uuid.New()
	connID := uuid.New()
	crypto := newTestCrypto(t)
	existingConfig, err := crypto.Seal(context.Background(), []byte(`{"host":"original"}`), aad(orgID))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	existing := db.Connector{
		ID:              connID,
		OrganizationID:  orgID,
		Name:            "old-name",
		Type:            string(TypeGeneric),
		Status:          "active",
		EncryptedConfig: existingConfig,
	}

	var captured db.UpdateConnectorParams
	store := &mockConnectorStore{
		getConnector: func(ctx context.Context, arg db.GetConnectorParams) (db.Connector, error) {
			return existing, nil
		},
		updateConnector: func(ctx context.Context, arg db.UpdateConnectorParams) (db.Connector, error) {
			captured = arg
			return existing, nil
		},
	}
	svc := NewService(store, crypto, newTestAudit(&spyQuerier{}), allowAllLimiter(), NewRegistry(), newTestLog())

	newName := "new-name"
	if _, err := svc.Update(context.Background(), orgID, uuid.New(), connID, &newName, nil, nil); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if captured.Name != "new-name" {
		t.Errorf("captured name = %q, want %q", captured.Name, "new-name")
	}
	if captured.Status != "active" {
		t.Errorf("captured status = %q, want existing status %q", captured.Status, "active")
	}
	if !bytes.Equal(captured.EncryptedConfig, existingConfig) {
		t.Errorf("captured config was re-sealed; want byte-identical to existing config")
	}
}

func TestService_Update_Config_ReSealsUnderFreshKey(t *testing.T) {
	orgID := uuid.New()
	connID := uuid.New()
	crypto := newTestCrypto(t)
	existingConfig, err := crypto.Seal(context.Background(), []byte(`{"host":"original"}`), aad(orgID))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	existing := db.Connector{
		ID:              connID,
		OrganizationID:  orgID,
		Name:            "name",
		Type:            string(TypeGeneric),
		Status:          "active",
		EncryptedConfig: existingConfig,
	}

	var captured db.UpdateConnectorParams
	store := &mockConnectorStore{
		getConnector: func(ctx context.Context, arg db.GetConnectorParams) (db.Connector, error) {
			return existing, nil
		},
		updateConnector: func(ctx context.Context, arg db.UpdateConnectorParams) (db.Connector, error) {
			captured = arg
			return existing, nil
		},
	}
	spy := &spyQuerier{}
	svc := NewService(store, crypto, newTestAudit(spy), allowAllLimiter(), NewRegistry(), newTestLog())

	newConfig := map[string]any{"host": "rotated.example.com"}
	if _, err := svc.Update(context.Background(), orgID, uuid.New(), connID, nil, nil, newConfig); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if bytes.Equal(captured.EncryptedConfig, existingConfig) {
		t.Fatal("config was not re-sealed despite a new config being supplied")
	}
	opened, err := crypto.Open(context.Background(), captured.EncryptedConfig, aad(orgID))
	if err != nil {
		t.Fatalf("Open new config: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(opened, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["host"] != "rotated.example.com" {
		t.Fatalf("opened config = %v, want host=rotated.example.com", got)
	}

	if len(spy.auditCalls) != 1 {
		t.Fatalf("audit calls = %d, want 1", len(spy.auditCalls))
	}
	var meta map[string]string
	if err := json.Unmarshal(spy.auditCalls[0].Metadata, &meta); err != nil {
		t.Fatalf("unmarshal audit metadata: %v", err)
	}
	if meta["config_rotated"] != "true" {
		t.Fatalf("audit metadata = %v, want config_rotated=true", meta)
	}
}

// ---- Delete ----

func TestService_Delete_NotFound(t *testing.T) {
	spy := &spyQuerier{}
	store := &mockConnectorStore{
		deleteConnector: func(ctx context.Context, arg db.DeleteConnectorParams) (int64, error) {
			return 0, nil
		},
	}
	svc := NewService(store, newTestCrypto(t), newTestAudit(spy), allowAllLimiter(), NewRegistry(), newTestLog())

	err := svc.Delete(context.Background(), uuid.New(), uuid.New(), uuid.New())
	if code := appErrorCode(t, err); code != apperror.NotFound {
		t.Fatalf("code = %q, want %q", code, apperror.NotFound)
	}
	if len(spy.auditCalls) != 0 {
		t.Fatalf("audit calls = %d, want 0 (no row was actually deleted)", len(spy.auditCalls))
	}
}

func TestService_Delete_HappyPath(t *testing.T) {
	spy := &spyQuerier{}
	store := &mockConnectorStore{
		deleteConnector: func(ctx context.Context, arg db.DeleteConnectorParams) (int64, error) {
			return 1, nil
		},
	}
	svc := NewService(store, newTestCrypto(t), newTestAudit(spy), allowAllLimiter(), NewRegistry(), newTestLog())

	if err := svc.Delete(context.Background(), uuid.New(), uuid.New(), uuid.New()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(spy.auditCalls) != 1 {
		t.Fatalf("audit calls = %d, want 1", len(spy.auditCalls))
	}
	if spy.auditCalls[0].Action != auditlog.ActionConnectorDeleted {
		t.Errorf("audit action = %q, want %q", spy.auditCalls[0].Action, auditlog.ActionConnectorDeleted)
	}
}

// ---- CheckHealth ----

func TestService_CheckHealth_UnsupportedType(t *testing.T) {
	orgID := uuid.New()
	connID := uuid.New()
	store := &mockConnectorStore{
		getConnector: func(ctx context.Context, arg db.GetConnectorParams) (db.Connector, error) {
			return db.Connector{ID: connID, OrganizationID: orgID, Type: string(TypeGeneric)}, nil
		},
	}
	svc := NewService(store, newTestCrypto(t), newTestAudit(&spyQuerier{}), allowAllLimiter(), NewRegistry(), newTestLog())

	_, err := svc.CheckHealth(context.Background(), orgID, connID)
	if code := appErrorCode(t, err); code != apperror.HealthCheckUnsupported {
		t.Fatalf("code = %q, want %q", code, apperror.HealthCheckUnsupported)
	}
}

func TestService_CheckHealth_HealthyWritesActive(t *testing.T) {
	orgID := uuid.New()
	connID := uuid.New()
	crypto := newTestCrypto(t)
	config := map[string]any{"host": "upstream.example.com"}
	plaintext, _ := json.Marshal(config)
	sealedConfig, err := crypto.Seal(context.Background(), plaintext, aad(orgID))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	checker := &fakeChecker{typ: TypeGeneric, err: nil}
	var capturedStatus string
	store := &mockConnectorStore{
		getConnector: func(ctx context.Context, arg db.GetConnectorParams) (db.Connector, error) {
			return db.Connector{ID: connID, OrganizationID: orgID, Type: string(TypeGeneric), EncryptedConfig: sealedConfig}, nil
		},
		updateConnectorHealth: func(ctx context.Context, arg db.UpdateConnectorHealthParams) (db.Connector, error) {
			capturedStatus = arg.Status
			return db.Connector{ID: connID, OrganizationID: orgID, Status: arg.Status}, nil
		},
	}
	svc := NewService(store, crypto, newTestAudit(&spyQuerier{}), allowAllLimiter(), NewRegistry(checker), newTestLog())

	if _, err := svc.CheckHealth(context.Background(), orgID, connID); err != nil {
		t.Fatalf("CheckHealth: %v", err)
	}
	if capturedStatus != string(StatusActive) {
		t.Errorf("status = %q, want %q", capturedStatus, StatusActive)
	}
	if checker.gotConfig["host"] != "upstream.example.com" {
		t.Fatalf("checker received config = %v, want the decrypted config", checker.gotConfig)
	}
}

func TestService_CheckHealth_FailureWritesError(t *testing.T) {
	orgID := uuid.New()
	connID := uuid.New()
	crypto := newTestCrypto(t)
	sealedConfig, err := crypto.Seal(context.Background(), []byte(`{"host":"upstream"}`), aad(orgID))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	checker := &fakeChecker{typ: TypeGeneric, err: errors.New("upstream unreachable")}
	var capturedStatus string
	store := &mockConnectorStore{
		getConnector: func(ctx context.Context, arg db.GetConnectorParams) (db.Connector, error) {
			return db.Connector{ID: connID, OrganizationID: orgID, Type: string(TypeGeneric), EncryptedConfig: sealedConfig}, nil
		},
		updateConnectorHealth: func(ctx context.Context, arg db.UpdateConnectorHealthParams) (db.Connector, error) {
			capturedStatus = arg.Status
			return db.Connector{ID: connID, OrganizationID: orgID, Status: arg.Status}, nil
		},
	}
	svc := NewService(store, crypto, newTestAudit(&spyQuerier{}), allowAllLimiter(), NewRegistry(checker), newTestLog())

	if _, err := svc.CheckHealth(context.Background(), orgID, connID); err != nil {
		t.Fatalf("CheckHealth: %v", err)
	}
	if capturedStatus != string(StatusError) {
		t.Errorf("status = %q, want %q", capturedStatus, StatusError)
	}
}

func TestService_CheckHealth_NotFound(t *testing.T) {
	store := &mockConnectorStore{
		getConnector: func(ctx context.Context, arg db.GetConnectorParams) (db.Connector, error) {
			return db.Connector{}, pgx.ErrNoRows
		},
	}
	svc := NewService(store, newTestCrypto(t), newTestAudit(&spyQuerier{}), allowAllLimiter(), NewRegistry(), newTestLog())

	_, err := svc.CheckHealth(context.Background(), uuid.New(), uuid.New())
	if code := appErrorCode(t, err); code != apperror.NotFound {
		t.Fatalf("code = %q, want %q", code, apperror.NotFound)
	}
}

func TestService_CheckHealth_DecryptFailurePropagates(t *testing.T) {
	orgID := uuid.New()
	connID := uuid.New()
	checker := &fakeChecker{typ: TypeGeneric}
	store := &mockConnectorStore{
		getConnector: func(ctx context.Context, arg db.GetConnectorParams) (db.Connector, error) {
			return db.Connector{ID: connID, OrganizationID: orgID, Type: string(TypeGeneric), EncryptedConfig: json.RawMessage(`{"v":1,"kid":"x","dek":"AA==","ct":"AA=="}`)}, nil
		},
	}
	svc := NewService(store, &failingSealer{err: errors.New("decrypt boom")}, newTestAudit(&spyQuerier{}), allowAllLimiter(), NewRegistry(checker), newTestLog())

	_, err := svc.CheckHealth(context.Background(), orgID, connID)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var appErr *apperror.Error
	if errors.As(err, &appErr) {
		t.Fatalf("expected a plain (non-apperror) error, got *apperror.Error(%s)", appErr.Code)
	}
}
