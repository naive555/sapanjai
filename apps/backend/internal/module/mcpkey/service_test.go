package mcpkey

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/shared/apperror"
)

// ---- hand-mocked mcpKeyStore ----

type mockMCPKeyStore struct {
	createMCPKey     func(ctx context.Context, arg db.CreateMCPKeyParams) (db.McpApiKey, error)
	getMCPKeyByName  func(ctx context.Context, arg db.GetMCPKeyByNameParams) (db.McpApiKey, error)
	listMCPKeysByOrg func(ctx context.Context, organizationID uuid.UUID) ([]db.McpApiKey, error)
	revokeMCPKey     func(ctx context.Context, arg db.RevokeMCPKeyParams) (int64, error)
}

func (m *mockMCPKeyStore) CreateMCPKey(ctx context.Context, arg db.CreateMCPKeyParams) (db.McpApiKey, error) {
	return m.createMCPKey(ctx, arg)
}
func (m *mockMCPKeyStore) GetMCPKeyByName(ctx context.Context, arg db.GetMCPKeyByNameParams) (db.McpApiKey, error) {
	return m.getMCPKeyByName(ctx, arg)
}
func (m *mockMCPKeyStore) ListMCPKeysByOrg(ctx context.Context, organizationID uuid.UUID) ([]db.McpApiKey, error) {
	return m.listMCPKeysByOrg(ctx, organizationID)
}
func (m *mockMCPKeyStore) RevokeMCPKey(ctx context.Context, arg db.RevokeMCPKeyParams) (int64, error) {
	return m.revokeMCPKey(ctx, arg)
}

var _ mcpKeyStore = (*mockMCPKeyStore)(nil)

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

	var captured db.CreateMCPKeyParams
	store := &mockMCPKeyStore{
		getMCPKeyByName: func(ctx context.Context, arg db.GetMCPKeyByNameParams) (db.McpApiKey, error) {
			return db.McpApiKey{}, pgx.ErrNoRows
		},
		createMCPKey: func(ctx context.Context, arg db.CreateMCPKeyParams) (db.McpApiKey, error) {
			captured = arg
			return db.McpApiKey{
				ID:             uuid.New(),
				OrganizationID: arg.OrganizationID,
				UserID:         arg.UserID,
				Name:           arg.Name,
				KeyHash:        arg.KeyHash,
				ExpiresAt:      arg.ExpiresAt,
				CreatedAt:      time.Now(),
			}, nil
		},
	}
	svc := NewService(store, newTestLog())

	row, rawToken, err := svc.Create(context.Background(), orgID, actorID, "laptop", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if !strings.HasPrefix(rawToken, "sk_live_") {
		t.Fatalf("rawToken = %q, want sk_live_ prefix", rawToken)
	}
	if rawToken == "sk_live_" {
		t.Fatalf("rawToken has no random suffix: %q", rawToken)
	}

	if captured.OrganizationID != orgID || captured.UserID != actorID || captured.Name != "laptop" {
		t.Fatalf("captured params = %+v, want org=%s user=%s name=laptop", captured, orgID, actorID)
	}

	// The store must only ever see the hash, never the raw token.
	if captured.KeyHash == rawToken {
		t.Fatal("stored KeyHash equals the raw token")
	}
	sum := sha256.Sum256([]byte(rawToken))
	wantHash := hex.EncodeToString(sum[:])
	if captured.KeyHash != wantHash {
		t.Fatalf("KeyHash = %q, want sha256(rawToken) = %q", captured.KeyHash, wantHash)
	}

	if captured.ExpiresAt.Valid {
		t.Fatalf("ExpiresAt = %+v, want invalid (no expiry) when expiresInDays is nil", captured.ExpiresAt)
	}

	if row.Name != "laptop" {
		t.Fatalf("row.Name = %q, want laptop", row.Name)
	}
}

func TestService_Create_TokensAreUnique(t *testing.T) {
	store := &mockMCPKeyStore{
		getMCPKeyByName: func(ctx context.Context, arg db.GetMCPKeyByNameParams) (db.McpApiKey, error) {
			return db.McpApiKey{}, pgx.ErrNoRows
		},
		createMCPKey: func(ctx context.Context, arg db.CreateMCPKeyParams) (db.McpApiKey, error) {
			return db.McpApiKey{ID: uuid.New(), Name: arg.Name, KeyHash: arg.KeyHash}, nil
		},
	}
	svc := NewService(store, newTestLog())

	_, token1, err := svc.Create(context.Background(), uuid.New(), uuid.New(), "a", nil)
	if err != nil {
		t.Fatalf("Create 1: %v", err)
	}
	_, token2, err := svc.Create(context.Background(), uuid.New(), uuid.New(), "b", nil)
	if err != nil {
		t.Fatalf("Create 2: %v", err)
	}
	if token1 == token2 {
		t.Fatalf("two calls to Create produced the same token: %q", token1)
	}
}

func TestService_Create_ExpiresInDaysSetsAbsoluteExpiry(t *testing.T) {
	var captured db.CreateMCPKeyParams
	store := &mockMCPKeyStore{
		getMCPKeyByName: func(ctx context.Context, arg db.GetMCPKeyByNameParams) (db.McpApiKey, error) {
			return db.McpApiKey{}, pgx.ErrNoRows
		},
		createMCPKey: func(ctx context.Context, arg db.CreateMCPKeyParams) (db.McpApiKey, error) {
			captured = arg
			return db.McpApiKey{ID: uuid.New(), Name: arg.Name, ExpiresAt: arg.ExpiresAt}, nil
		},
	}
	svc := NewService(store, newTestLog())

	days := 30
	before := time.Now().Add(30 * 24 * time.Hour)
	if _, _, err := svc.Create(context.Background(), uuid.New(), uuid.New(), "expiring", &days); err != nil {
		t.Fatalf("Create: %v", err)
	}
	after := time.Now().Add(30 * 24 * time.Hour)

	if !captured.ExpiresAt.Valid {
		t.Fatal("ExpiresAt.Valid = false, want true")
	}
	if captured.ExpiresAt.Time.Before(before.Add(-time.Minute)) || captured.ExpiresAt.Time.After(after.Add(time.Minute)) {
		t.Fatalf("ExpiresAt = %v, want ~30 days from now (between %v and %v)", captured.ExpiresAt.Time, before, after)
	}
}

func TestService_Create_NameTaken(t *testing.T) {
	store := &mockMCPKeyStore{
		getMCPKeyByName: func(ctx context.Context, arg db.GetMCPKeyByNameParams) (db.McpApiKey, error) {
			return db.McpApiKey{ID: uuid.New()}, nil
		},
	}
	svc := NewService(store, newTestLog())

	_, _, err := svc.Create(context.Background(), uuid.New(), uuid.New(), "dup-name", nil)
	if code := appErrorCode(t, err); code != apperror.MCPKeyNameTaken {
		t.Fatalf("code = %q, want %q", code, apperror.MCPKeyNameTaken)
	}
}

func TestService_Create_NameLookupErrorPropagates(t *testing.T) {
	wantErr := errors.New("boom: db unavailable")
	store := &mockMCPKeyStore{
		getMCPKeyByName: func(ctx context.Context, arg db.GetMCPKeyByNameParams) (db.McpApiKey, error) {
			return db.McpApiKey{}, wantErr
		},
	}
	svc := NewService(store, newTestLog())

	_, _, err := svc.Create(context.Background(), uuid.New(), uuid.New(), "x", nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	var appErr *apperror.Error
	if errors.As(err, &appErr) {
		t.Fatalf("expected a plain error, got *apperror.Error(%s)", appErr.Code)
	}
}

func TestService_Create_InsertErrorPropagates(t *testing.T) {
	wantErr := errors.New("boom: insert failed")
	store := &mockMCPKeyStore{
		getMCPKeyByName: func(ctx context.Context, arg db.GetMCPKeyByNameParams) (db.McpApiKey, error) {
			return db.McpApiKey{}, pgx.ErrNoRows
		},
		createMCPKey: func(ctx context.Context, arg db.CreateMCPKeyParams) (db.McpApiKey, error) {
			return db.McpApiKey{}, wantErr
		},
	}
	svc := NewService(store, newTestLog())

	_, rawToken, err := svc.Create(context.Background(), uuid.New(), uuid.New(), "x", nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if rawToken != "" {
		t.Fatalf("rawToken = %q, want empty on error", rawToken)
	}
}

// ---- List ----

func TestService_List_ScopesToOrg(t *testing.T) {
	orgID := uuid.New()
	var captured uuid.UUID
	store := &mockMCPKeyStore{
		listMCPKeysByOrg: func(ctx context.Context, organizationID uuid.UUID) ([]db.McpApiKey, error) {
			captured = organizationID
			return nil, nil
		},
	}
	svc := NewService(store, newTestLog())

	if _, err := svc.List(context.Background(), orgID); err != nil {
		t.Fatalf("List: %v", err)
	}
	if captured != orgID {
		t.Fatalf("captured org = %s, want %s", captured, orgID)
	}
}

// ---- Revoke ----

func TestService_Revoke_NotFound(t *testing.T) {
	store := &mockMCPKeyStore{
		revokeMCPKey: func(ctx context.Context, arg db.RevokeMCPKeyParams) (int64, error) {
			return 0, nil
		},
	}
	svc := NewService(store, newTestLog())

	err := svc.Revoke(context.Background(), uuid.New(), uuid.New())
	if code := appErrorCode(t, err); code != apperror.MCPKeyNotFound {
		t.Fatalf("code = %q, want %q", code, apperror.MCPKeyNotFound)
	}
}

func TestService_Revoke_HappyPath(t *testing.T) {
	orgID := uuid.New()
	keyID := uuid.New()
	var captured db.RevokeMCPKeyParams
	store := &mockMCPKeyStore{
		revokeMCPKey: func(ctx context.Context, arg db.RevokeMCPKeyParams) (int64, error) {
			captured = arg
			return 1, nil
		},
	}
	svc := NewService(store, newTestLog())

	if err := svc.Revoke(context.Background(), orgID, keyID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if captured.ID != keyID || captured.OrganizationID != orgID {
		t.Fatalf("captured params = %+v, want id=%s org=%s", captured, keyID, orgID)
	}
}

func TestService_Revoke_ErrorPropagates(t *testing.T) {
	wantErr := errors.New("boom: update failed")
	store := &mockMCPKeyStore{
		revokeMCPKey: func(ctx context.Context, arg db.RevokeMCPKeyParams) (int64, error) {
			return 0, wantErr
		},
	}
	svc := NewService(store, newTestLog())

	err := svc.Revoke(context.Background(), uuid.New(), uuid.New())
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

// ---- HashToken sanity (belt-and-braces on the documented deviation) ----
//
// Exported in step 3 (docs/07-sheets-adapter-plan.md) so
// internal/middleware.RequireMCPKey can hash a presented bearer token with
// the exact same function used at mint time; this test still exercises it
// through the package's public surface.

func TestHashToken_DeterministicAndNotBcrypt(t *testing.T) {
	token := "sk_live_abc123"
	h1 := HashToken(token)
	h2 := HashToken(token)
	if h1 != h2 {
		t.Fatalf("HashToken is not deterministic: %q != %q (bcrypt would salt differently each call)", h1, h2)
	}
	sum := sha256.Sum256([]byte(token))
	if h1 != hex.EncodeToString(sum[:]) {
		t.Fatalf("HashToken(%q) = %q, want sha256 hex digest", token, h1)
	}
}
