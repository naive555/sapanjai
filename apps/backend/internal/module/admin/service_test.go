package admin

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/module/auditlog"
	"github.com/sapanjai/backend/internal/shared/apperror"
)

// mockAdminStore embeds the (nil) adminStore interface so any method this
// test doesn't override panics loudly if accidentally called, rather than
// silently returning a zero value — the same "hand-mock the narrow
// interface" pattern mcpkey/connector's own service tests use, adapted for
// an interface this wide.
type mockAdminStore struct {
	adminStore

	getUserByID func(ctx context.Context, id uuid.UUID) (db.User, error)

	adminGetOrganizationByID func(ctx context.Context, id uuid.UUID) (db.Organization, error)
	adminDeleteOrganization  func(ctx context.Context, id uuid.UUID) error
	adminDeleteOrgCalls      int

	adminSetOrgCustomLimits func(ctx context.Context, arg db.AdminSetOrgCustomLimitsParams) (int64, error)

	countSuperadmins func(ctx context.Context) (int64, error)
	withTx           func(ctx context.Context, fn func(q db.Querier) error) error

	adminGetPlanByID              func(ctx context.Context, id uuid.UUID) (db.Plan, error)
	adminCreatePlan               func(ctx context.Context, arg db.AdminCreatePlanParams) (db.Plan, error)
	adminUpdatePlan               func(ctx context.Context, arg db.AdminUpdatePlanParams) (db.Plan, error)
	adminDeletePlan               func(ctx context.Context, id uuid.UUID) error
	adminDeletePlanCalls          int
	adminCountSubscriptionsByPlan func(ctx context.Context, planID uuid.UUID) (int64, error)

	upsertUserTOTPSecret        func(ctx context.Context, arg db.UpsertUserTOTPSecretParams) error
	getUserTOTP                 func(ctx context.Context, userID uuid.UUID) (db.UserTotp, error)
	confirmUserTOTP             func(ctx context.Context, arg db.ConfirmUserTOTPParams) error
	updateUserTOTPRecoveryCodes func(ctx context.Context, arg db.UpdateUserTOTPRecoveryCodesParams) error
}

func (m *mockAdminStore) GetUserByID(ctx context.Context, id uuid.UUID) (db.User, error) {
	return m.getUserByID(ctx, id)
}

func (m *mockAdminStore) AdminGetOrganizationByID(ctx context.Context, id uuid.UUID) (db.Organization, error) {
	return m.adminGetOrganizationByID(ctx, id)
}

func (m *mockAdminStore) AdminDeleteOrganization(ctx context.Context, id uuid.UUID) error {
	m.adminDeleteOrgCalls++
	if m.adminDeleteOrganization != nil {
		return m.adminDeleteOrganization(ctx, id)
	}
	return nil
}

func (m *mockAdminStore) AdminSetOrgCustomLimits(ctx context.Context, arg db.AdminSetOrgCustomLimitsParams) (int64, error) {
	return m.adminSetOrgCustomLimits(ctx, arg)
}

func (m *mockAdminStore) CountSuperadmins(ctx context.Context) (int64, error) {
	return m.countSuperadmins(ctx)
}

func (m *mockAdminStore) WithTx(ctx context.Context, fn func(q db.Querier) error) error {
	return m.withTx(ctx, fn)
}

func (m *mockAdminStore) AdminGetPlanByID(ctx context.Context, id uuid.UUID) (db.Plan, error) {
	return m.adminGetPlanByID(ctx, id)
}

func (m *mockAdminStore) AdminCreatePlan(ctx context.Context, arg db.AdminCreatePlanParams) (db.Plan, error) {
	return m.adminCreatePlan(ctx, arg)
}

func (m *mockAdminStore) AdminUpdatePlan(ctx context.Context, arg db.AdminUpdatePlanParams) (db.Plan, error) {
	return m.adminUpdatePlan(ctx, arg)
}

func (m *mockAdminStore) AdminDeletePlan(ctx context.Context, id uuid.UUID) error {
	m.adminDeletePlanCalls++
	if m.adminDeletePlan != nil {
		return m.adminDeletePlan(ctx, id)
	}
	return nil
}

func (m *mockAdminStore) AdminCountSubscriptionsByPlan(ctx context.Context, planID uuid.UUID) (int64, error) {
	return m.adminCountSubscriptionsByPlan(ctx, planID)
}

func (m *mockAdminStore) UpsertUserTOTPSecret(ctx context.Context, arg db.UpsertUserTOTPSecretParams) error {
	return m.upsertUserTOTPSecret(ctx, arg)
}

func (m *mockAdminStore) GetUserTOTP(ctx context.Context, userID uuid.UUID) (db.UserTotp, error) {
	return m.getUserTOTP(ctx, userID)
}

func (m *mockAdminStore) ConfirmUserTOTP(ctx context.Context, arg db.ConfirmUserTOTPParams) error {
	return m.confirmUserTOTP(ctx, arg)
}

func (m *mockAdminStore) UpdateUserTOTPRecoveryCodes(ctx context.Context, arg db.UpdateUserTOTPRecoveryCodesParams) error {
	return m.updateUserTOTPRecoveryCodes(ctx, arg)
}

var _ adminStore = (*mockAdminStore)(nil)

// mockAdminAuth hand-mocks adminAuth for the reauth/ban rate-limit and
// Redis-ban-cache unit tests below.
type mockAdminAuth struct {
	getReauthAttempts func(ctx context.Context, userID uuid.UUID) (int, error)

	incrementReauthCalls int
	incrementReauthErr   error
	resetReauthCalls     int
	resetReauthErr       error

	banCalls   []uuid.UUID
	banErr     error
	unbanCalls []uuid.UUID
	unbanErr   error

	getTwoFactorAttempts func(ctx context.Context, userID uuid.UUID) (int, error)

	setTwoFactorVerifiedCalls []uuid.UUID
	setTwoFactorVerifiedErr   error

	incrementTwoFactorCalls int
	incrementTwoFactorErr   error
	resetTwoFactorCalls     int
	resetTwoFactorErr       error
}

func (m *mockAdminAuth) GetReauthAttempts(ctx context.Context, userID uuid.UUID) (int, error) {
	return m.getReauthAttempts(ctx, userID)
}

func (m *mockAdminAuth) IncrementReauthAttempts(ctx context.Context, userID uuid.UUID) (int64, error) {
	m.incrementReauthCalls++
	return int64(m.incrementReauthCalls), m.incrementReauthErr
}

func (m *mockAdminAuth) ResetReauthAttempts(ctx context.Context, userID uuid.UUID) error {
	m.resetReauthCalls++
	return m.resetReauthErr
}

func (m *mockAdminAuth) Ban(ctx context.Context, userID uuid.UUID) error {
	m.banCalls = append(m.banCalls, userID)
	return m.banErr
}

func (m *mockAdminAuth) Unban(ctx context.Context, userID uuid.UUID) error {
	m.unbanCalls = append(m.unbanCalls, userID)
	return m.unbanErr
}

func (m *mockAdminAuth) SetTwoFactorVerified(ctx context.Context, userID uuid.UUID) error {
	m.setTwoFactorVerifiedCalls = append(m.setTwoFactorVerifiedCalls, userID)
	return m.setTwoFactorVerifiedErr
}

func (m *mockAdminAuth) GetTwoFactorAttempts(ctx context.Context, userID uuid.UUID) (int, error) {
	if m.getTwoFactorAttempts == nil {
		return 0, nil
	}
	return m.getTwoFactorAttempts(ctx, userID)
}

func (m *mockAdminAuth) IncrementTwoFactorAttempts(ctx context.Context, userID uuid.UUID) (int64, error) {
	m.incrementTwoFactorCalls++
	return int64(m.incrementTwoFactorCalls), m.incrementTwoFactorErr
}

func (m *mockAdminAuth) ResetTwoFactorAttempts(ctx context.Context, userID uuid.UUID) error {
	m.resetTwoFactorCalls++
	return m.resetTwoFactorErr
}

var _ adminAuth = (*mockAdminAuth)(nil)

// mockCountCache is a simple in-memory countCache stand-in for
// cachedCount's unit tests below.
type mockCountCache struct {
	values map[string]int64
	getErr error
	setErr error
	gets   int
}

func (m *mockCountCache) Get(ctx context.Context, filterKey string) (int64, bool, error) {
	m.gets++
	if m.getErr != nil {
		return 0, false, m.getErr
	}
	v, ok := m.values[filterKey]
	return v, ok, nil
}

func (m *mockCountCache) Set(ctx context.Context, filterKey string, count int64, ttl time.Duration) error {
	if m.setErr != nil {
		return m.setErr
	}
	if m.values == nil {
		m.values = map[string]int64{}
	}
	m.values[filterKey] = count
	return nil
}

func (m *mockCountCache) UsedMemoryHuman(ctx context.Context) (string, error) {
	return "", nil
}

var _ countCache = (*mockCountCache)(nil)

func TestService_Me_HappyPath(t *testing.T) {
	userID := uuid.New()
	role := "superadmin"
	name := "Alice"

	svc := NewService(&mockAdminStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			if id != userID {
				t.Fatalf("GetUserByID called with %s, want %s", id, userID)
			}
			return db.User{ID: userID, Email: "alice@example.com", DisplayName: &name, PlatformRole: &role}, nil
		},
	}, &mockCountCache{}, nil, nil, nil, nil, nil, nil)

	got, err := svc.Me(context.Background(), userID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Email != "alice@example.com" || got.PlatformRole != "superadmin" || got.DisplayName == nil || *got.DisplayName != "Alice" {
		t.Errorf("Me() = %+v, unexpected", got)
	}
}

func TestService_Me_NilPlatformRoleIsEmptyString(t *testing.T) {
	// Defensive case: RequirePlatformRole would already have rejected a
	// caller with no platform_role, so this row shape shouldn't reach Me
	// in production, but Me must not panic dereferencing a nil pointer if
	// it somehow does.
	userID := uuid.New()
	svc := NewService(&mockAdminStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{ID: userID, Email: "bob@example.com", PlatformRole: nil}, nil
		},
	}, &mockCountCache{}, nil, nil, nil, nil, nil, nil)

	got, err := svc.Me(context.Background(), userID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.PlatformRole != "" {
		t.Errorf("PlatformRole = %q, want empty string for a nil platform_role", got.PlatformRole)
	}
}

func TestService_Me_DatabaseErrorPropagates(t *testing.T) {
	dbErr := errors.New("connection reset")
	svc := NewService(&mockAdminStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{}, dbErr
		},
	}, &mockCountCache{}, nil, nil, nil, nil, nil, nil)

	_, err := svc.Me(context.Background(), uuid.New())
	if !errors.Is(err, dbErr) {
		t.Fatalf("expected the raw db error to propagate, got %v", err)
	}
}

// ---- cachedCount ----

func TestCachedCount_MissComputesAndCaches(t *testing.T) {
	cache := &mockCountCache{}
	svc := NewService(nil, cache, nil, nil, nil, nil, nil, nil)

	computed := 0
	compute := func() (int64, error) {
		computed++
		return 42, nil
	}

	n, err := svc.cachedCount(context.Background(), "key-a", compute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 42 {
		t.Errorf("count = %d, want 42", n)
	}
	if computed != 1 {
		t.Errorf("compute called %d times, want 1", computed)
	}
	if cache.values["key-a"] != 42 {
		t.Errorf("cache did not persist the computed value: %v", cache.values)
	}
}

func TestCachedCount_HitSkipsCompute(t *testing.T) {
	cache := &mockCountCache{values: map[string]int64{"key-a": 7}}
	svc := NewService(nil, cache, nil, nil, nil, nil, nil, nil)

	compute := func() (int64, error) {
		t.Fatal("compute should not be called on a cache hit")
		return 0, nil
	}

	n, err := svc.cachedCount(context.Background(), "key-a", compute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 7 {
		t.Errorf("count = %d, want the cached 7", n)
	}
}

func TestCachedCount_DifferentFilterKeysMissIndependently(t *testing.T) {
	// The whole point of requiring every filter in the key: two different
	// filter sets must never share a cached total.
	cache := &mockCountCache{values: map[string]int64{"key-a": 7}}
	svc := NewService(nil, cache, nil, nil, nil, nil, nil, nil)

	n, err := svc.cachedCount(context.Background(), "key-b", func() (int64, error) { return 99, nil })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 99 {
		t.Errorf("count = %d, want 99 for a distinct filter key", n)
	}
}

func TestCachedCount_CacheReadErrorFallsBackToCompute(t *testing.T) {
	cache := &mockCountCache{getErr: errors.New("redis unavailable")}
	svc := NewService(nil, cache, nil, nil, nil, nil, nil, nil)

	n, err := svc.cachedCount(context.Background(), "key-a", func() (int64, error) { return 5, nil })
	if err != nil {
		t.Fatalf("a Redis outage must not fail an admin list request, got %v", err)
	}
	if n != 5 {
		t.Errorf("count = %d, want 5 (computed directly)", n)
	}
}

func TestCachedCount_ComputeErrorPropagates(t *testing.T) {
	computeErr := errors.New("query failed")
	svc := NewService(nil, &mockCountCache{}, nil, nil, nil, nil, nil, nil)

	_, err := svc.cachedCount(context.Background(), "key-a", func() (int64, error) { return 0, computeErr })
	if !errors.Is(err, computeErr) {
		t.Fatalf("expected the compute error to propagate, got %v", err)
	}
}

// ---- buildActionPatterns ----

func TestBuildActionPatterns(t *testing.T) {
	tests := []struct {
		name    string
		actions []string
		want    []string
	}{
		{name: "nil for no filter", actions: nil, want: nil},
		{name: "nil for empty slice", actions: []string{}, want: nil},
		{name: "exact action passes through unescaped", actions: []string{"user.login"}, want: []string{"user.login"}},
		{name: "trailing star becomes a prefix pattern", actions: []string{"admin.*"}, want: []string{"admin.%"}},
		{
			name:    "mixed exact and prefix",
			actions: []string{"user.login", "mcp.*"},
			want:    []string{"user.login", "mcp.%"},
		},
		{
			name:    "literal percent and underscore are escaped so they can't be misread as wildcards",
			actions: []string{"weird_action%name"},
			want:    []string{`weird\_action\%name`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildActionPatterns(tt.actions)
			if len(got) != len(tt.want) {
				t.Fatalf("buildActionPatterns(%v) = %v, want %v", tt.actions, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("buildActionPatterns(%v)[%d] = %q, want %q", tt.actions, i, got[i], tt.want[i])
				}
			}
		})
	}
}

// ==== Phase 3: mutations ====

// spyQuerier records CreateAuditLog calls for assertion; it embeds the
// db.Querier interface unset so any other method panics if accidentally
// exercised — none of these tests should ever reach one. Mirrors
// internal/module/auth/service_test.go's own copy exactly (each module
// keeps its own rather than sharing one across packages).
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

func appErrorCode(t *testing.T, err error) string {
	t.Helper()
	var appErr *apperror.Error
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *apperror.Error, got %T: %v", err, err)
	}
	return appErr.Code
}

// adminBcryptHash is a bcrypt hash of "password123", computed once at
// bcrypt.MinCost (bcrypt.CompareHashAndPassword doesn't care what cost a
// hash was generated at, only tests generating fresh ones care about
// speed) and reused across every reauth-dependent test below.
var adminBcryptHash = mustBcryptHash("password123")

func mustBcryptHash(password string) string {
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		panic(err)
	}
	return string(h)
}

// alwaysAllowReauth is a mockAdminAuth whose GetReauthAttempts always
// reports zero — used by every guard-ordering test below that isn't itself
// testing the reauth step, so those tests can focus on the guard after it.
func alwaysAllowReauth() *mockAdminAuth {
	return &mockAdminAuth{getReauthAttempts: func(ctx context.Context, userID uuid.UUID) (int, error) { return 0, nil }}
}

// ---- reauth ----

func TestReauth_Success(t *testing.T) {
	adminID := uuid.New()
	auth := alwaysAllowReauth()
	svc := NewService(&mockAdminStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{ID: adminID, PasswordHash: adminBcryptHash}, nil
		},
	}, &mockCountCache{}, nil, nil, auth, nil, nil, nil)

	if err := svc.reauth(context.Background(), adminID, "password123"); err != nil {
		t.Fatalf("reauth() unexpected error: %v", err)
	}
	if auth.resetReauthCalls != 1 {
		t.Errorf("ResetReauthAttempts called %d times, want 1", auth.resetReauthCalls)
	}
	if auth.incrementReauthCalls != 0 {
		t.Errorf("IncrementReauthAttempts called %d times, want 0 on success", auth.incrementReauthCalls)
	}
}

func TestReauth_WrongPassword(t *testing.T) {
	adminID := uuid.New()
	auth := alwaysAllowReauth()
	svc := NewService(&mockAdminStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{ID: adminID, PasswordHash: adminBcryptHash}, nil
		},
	}, &mockCountCache{}, nil, nil, auth, nil, nil, nil)

	err := svc.reauth(context.Background(), adminID, "totally-wrong")
	if appErrorCode(t, err) != apperror.ReauthFailed {
		t.Fatalf("reauth() error = %v, want ReauthFailed", err)
	}
	if auth.incrementReauthCalls != 1 {
		t.Errorf("IncrementReauthAttempts called %d times, want 1", auth.incrementReauthCalls)
	}
	if auth.resetReauthCalls != 0 {
		t.Errorf("ResetReauthAttempts called %d times, want 0 on a failed attempt", auth.resetReauthCalls)
	}
}

func TestReauth_TooManyAttempts(t *testing.T) {
	adminID := uuid.New()
	auth := &mockAdminAuth{getReauthAttempts: func(ctx context.Context, userID uuid.UUID) (int, error) { return maxReauthAttempts, nil }}
	// getUserByID is intentionally left nil: exhausting the limiter must
	// short-circuit before the admin's row is ever loaded or a password
	// compared — calling the unset func would nil-panic and fail the test.
	svc := NewService(&mockAdminStore{}, &mockCountCache{}, nil, nil, auth, nil, nil, nil)

	err := svc.reauth(context.Background(), adminID, "password123")
	if appErrorCode(t, err) != apperror.TooManyAttempts {
		t.Fatalf("reauth() error = %v, want TooManyAttempts", err)
	}
}

// ---- validatePlanLimits ----

func TestValidatePlanLimits(t *testing.T) {
	tests := []struct {
		name    string
		limits  map[string]any
		wantErr bool
	}{
		{name: "valid", limits: map[string]any{"max_members": 5.0, "max_roles": 3.0, "max_connectors": 2.0}, wantErr: false},
		{name: "valid unlimited (-1)", limits: map[string]any{"max_members": -1.0, "max_roles": -1.0, "max_connectors": -1.0}, wantErr: false},
		{name: "extra integer key is fine", limits: map[string]any{"max_members": 5.0, "max_roles": 3.0, "max_connectors": 2.0, "extra": 7.0}, wantErr: false},
		{name: "missing a required key", limits: map[string]any{"max_members": 5.0, "max_roles": 3.0}, wantErr: true},
		{name: "non-integer required value", limits: map[string]any{"max_members": 5.5, "max_roles": 3.0, "max_connectors": 2.0}, wantErr: true},
		{name: "non-integer extra key", limits: map[string]any{"max_members": 5.0, "max_roles": 3.0, "max_connectors": 2.0, "extra": 1.5}, wantErr: true},
		{name: "non-numeric value", limits: map[string]any{"max_members": "five", "max_roles": 3.0, "max_connectors": 2.0}, wantErr: true},
		{name: "empty limits", limits: map[string]any{}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePlanLimits(tt.limits)
			if (err != nil) != tt.wantErr {
				t.Errorf("validatePlanLimits(%v) error = %v, wantErr %v", tt.limits, err, tt.wantErr)
			}
		})
	}
}

// ---- validateCustomLimits ----

// An org's custom_limits is a partial overlay, so unlike a plan's own
// limits it must accept a blob holding only the one key being overridden
// while still rejecting any non-integer value — the case that would
// otherwise make subscription.Service.EffectiveLimits silently drop the
// entire override at enforcement time.
func TestValidateCustomLimits(t *testing.T) {
	tests := []struct {
		name    string
		limits  map[string]any
		wantErr bool
	}{
		{name: "single key override, no required keys needed", limits: map[string]any{"max_members": 25.0}, wantErr: false},
		{name: "empty object overrides nothing", limits: map[string]any{}, wantErr: false},
		{name: "unlimited (-1)", limits: map[string]any{"max_connectors": -1.0}, wantErr: false},
		{name: "arbitrary key is fine if integral", limits: map[string]any{"max_widgets": 3.0}, wantErr: false},
		{name: "non-numeric value rejected", limits: map[string]any{"max_members": "ten"}, wantErr: true},
		{name: "fractional value rejected", limits: map[string]any{"max_members": 2.5}, wantErr: true},
		{name: "one bad key poisons the blob", limits: map[string]any{"max_members": 25.0, "max_roles": true}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCustomLimits(tt.limits)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateCustomLimits(%v) error = %v, wantErr %v", tt.limits, err, tt.wantErr)
			}
		})
	}
}

// ---- ChangePlatformRole guard ordering ----

func TestChangePlatformRole_CannotTargetSelf(t *testing.T) {
	adminID := uuid.New()
	store := &mockAdminStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{ID: adminID, PasswordHash: adminBcryptHash}, nil
		},
		// withTx left nil: reaching it would mean the self-target guard
		// didn't short-circuit before the write.
	}
	svc := NewService(store, &mockCountCache{}, nil, nil, alwaysAllowReauth(), nil, nil, nil)

	role := "support"
	err := svc.ChangePlatformRole(context.Background(), AdminContext{AdminID: adminID}, adminID, &role, "password123")
	if appErrorCode(t, err) != apperror.CannotTargetSelf {
		t.Fatalf("ChangePlatformRole() error = %v, want CannotTargetSelf", err)
	}
}

func TestChangePlatformRole_TargetNotFound(t *testing.T) {
	adminID, targetID := uuid.New(), uuid.New()
	store := &mockAdminStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			if id == adminID {
				return db.User{ID: adminID, PasswordHash: adminBcryptHash}, nil
			}
			return db.User{}, pgx.ErrNoRows
		},
	}
	svc := NewService(store, &mockCountCache{}, nil, nil, alwaysAllowReauth(), nil, nil, nil)

	role := "support"
	err := svc.ChangePlatformRole(context.Background(), AdminContext{AdminID: adminID}, targetID, &role, "password123")
	if appErrorCode(t, err) != apperror.UserNotFound {
		t.Fatalf("ChangePlatformRole() error = %v, want UserNotFound (a missing target must not be a silent no-op)", err)
	}
}

func TestChangePlatformRole_SuperadminLimit(t *testing.T) {
	adminID, targetID := uuid.New(), uuid.New()
	store := &mockAdminStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			if id == adminID {
				return db.User{ID: adminID, PasswordHash: adminBcryptHash}, nil
			}
			return db.User{ID: targetID, Email: "target@example.com"}, nil
		},
		countSuperadmins: func(ctx context.Context) (int64, error) { return superadminCap, nil },
		// withTx left nil: the cap must be enforced before any write.
	}
	svc := NewService(store, &mockCountCache{}, nil, nil, alwaysAllowReauth(), nil, nil, nil)

	role := "superadmin"
	err := svc.ChangePlatformRole(context.Background(), AdminContext{AdminID: adminID}, targetID, &role, "password123")
	if appErrorCode(t, err) != apperror.SuperadminLimit {
		t.Fatalf("ChangePlatformRole() error = %v, want SuperadminLimit", err)
	}
}

func TestChangePlatformRole_DemotingPastCapIsNotBlocked(t *testing.T) {
	// The cap only applies when PROMOTING to superadmin — demoting one
	// (role == nil, or "support") must never consult CountSuperadmins at
	// all, let alone be blocked by it.
	adminID, targetID := uuid.New(), uuid.New()
	var tx *mockAdminTxQuerier
	store := &mockAdminStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			if id == adminID {
				return db.User{ID: adminID, PasswordHash: adminBcryptHash}, nil
			}
			return db.User{ID: targetID, Email: "target@example.com"}, nil
		},
		// countSuperadmins left nil: calling it would fail this test.
		withTx: withMockAdminTx(&tx),
	}
	svc := NewService(store, &mockCountCache{}, newTestAudit(&spyQuerier{}), nil, alwaysAllowReauth(), nil, nil, slog.New(slog.NewTextHandler(os.Stdout, nil)))

	if err := svc.ChangePlatformRole(context.Background(), AdminContext{AdminID: adminID}, targetID, nil, "password123"); err != nil {
		t.Fatalf("ChangePlatformRole() unexpected error: %v", err)
	}
	if len(tx.setUserPlatformRoleCalls) != 1 || tx.setUserPlatformRoleCalls[0].PlatformRole != nil {
		t.Errorf("setUserPlatformRoleCalls = %v, want one call with a nil role", tx.setUserPlatformRoleCalls)
	}
}

func TestChangePlatformRole_HappyPath_RevokesSessionsAndAudits(t *testing.T) {
	adminID, targetID := uuid.New(), uuid.New()
	var tx *mockAdminTxQuerier
	store := &mockAdminStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			if id == adminID {
				return db.User{ID: adminID, PasswordHash: adminBcryptHash}, nil
			}
			return db.User{ID: targetID, Email: "target@example.com"}, nil
		},
		countSuperadmins: func(ctx context.Context) (int64, error) { return 1, nil },
		withTx:           withMockAdminTx(&tx),
	}
	spy := &spyQuerier{}
	svc := NewService(store, &mockCountCache{}, newTestAudit(spy), nil, alwaysAllowReauth(), nil, nil, slog.New(slog.NewTextHandler(os.Stdout, nil)))

	role := "superadmin"
	if err := svc.ChangePlatformRole(context.Background(), AdminContext{AdminID: adminID, IP: "203.0.113.5", UserAgent: "test-agent"}, targetID, &role, "password123"); err != nil {
		t.Fatalf("ChangePlatformRole() unexpected error: %v", err)
	}

	if len(tx.setUserPlatformRoleCalls) != 1 || tx.setUserPlatformRoleCalls[0].ID != targetID || *tx.setUserPlatformRoleCalls[0].PlatformRole != "superadmin" {
		t.Errorf("setUserPlatformRoleCalls = %+v, want one superadmin grant for %s", tx.setUserPlatformRoleCalls, targetID)
	}
	if len(tx.revokeAllUserSessionsIDs) != 1 || tx.revokeAllUserSessionsIDs[0] != targetID {
		t.Errorf("revokeAllUserSessionsIDs = %v, want [%s]", tx.revokeAllUserSessionsIDs, targetID)
	}
	if len(spy.auditCalls) != 1 || spy.auditCalls[0].Action != auditlog.ActionAdminPlatformRoleChanged {
		t.Fatalf("auditCalls = %+v, want one admin.platform_role.changed entry", spy.auditCalls)
	}
}

// ---- SetBan guard ordering + the ban/unban Redis asymmetry ----

func TestSetBan_CannotTargetSelf(t *testing.T) {
	adminID := uuid.New()
	store := &mockAdminStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{ID: adminID, PasswordHash: adminBcryptHash}, nil
		},
	}
	svc := NewService(store, &mockCountCache{}, nil, nil, alwaysAllowReauth(), nil, nil, nil)

	err := svc.SetBan(context.Background(), AdminContext{AdminID: adminID}, adminID, true, nil, "password123")
	if appErrorCode(t, err) != apperror.CannotTargetSelf {
		t.Fatalf("SetBan() error = %v, want CannotTargetSelf", err)
	}
}

func TestSetBan_TargetIsPlatformStaff(t *testing.T) {
	adminID, targetID := uuid.New(), uuid.New()
	supportRole := "support"
	store := &mockAdminStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			if id == adminID {
				return db.User{ID: adminID, PasswordHash: adminBcryptHash}, nil
			}
			return db.User{ID: targetID, Email: "staff@example.com", PlatformRole: &supportRole}, nil
		},
		// withTx left nil: a platform-staff target must be refused before
		// any write, ban or unban alike.
	}
	svc := NewService(store, &mockCountCache{}, nil, nil, alwaysAllowReauth(), nil, nil, nil)

	err := svc.SetBan(context.Background(), AdminContext{AdminID: adminID}, targetID, true, nil, "password123")
	if appErrorCode(t, err) != apperror.TargetIsPlatformStaff {
		t.Fatalf("SetBan() error = %v, want TargetIsPlatformStaff", err)
	}
}

func TestSetBan_HappyPath_BansAndRevokesSessions(t *testing.T) {
	adminID, targetID := uuid.New(), uuid.New()
	var tx *mockAdminTxQuerier
	store := &mockAdminStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			if id == adminID {
				return db.User{ID: adminID, PasswordHash: adminBcryptHash}, nil
			}
			return db.User{ID: targetID, Email: "target@example.com"}, nil
		},
		withTx: withMockAdminTx(&tx),
	}
	auth := alwaysAllowReauth()
	spy := &spyQuerier{}
	svc := NewService(store, &mockCountCache{}, newTestAudit(spy), nil, auth, nil, nil, slog.New(slog.NewTextHandler(os.Stdout, nil)))

	reason := "fraud"
	if err := svc.SetBan(context.Background(), AdminContext{AdminID: adminID}, targetID, true, &reason, "password123"); err != nil {
		t.Fatalf("SetBan() unexpected error: %v", err)
	}

	if len(tx.setUserBanCalls) != 1 || tx.setUserBanCalls[0].ID != targetID || !tx.setUserBanCalls[0].BannedAt.Valid {
		t.Errorf("setUserBanCalls = %+v, want one ban for %s with BannedAt set", tx.setUserBanCalls, targetID)
	}
	if tx.setUserBanCalls[0].BanReason == nil || *tx.setUserBanCalls[0].BanReason != reason {
		t.Errorf("BanReason = %v, want %q", tx.setUserBanCalls[0].BanReason, reason)
	}
	if len(tx.revokeAllUserSessionsIDs) != 1 || tx.revokeAllUserSessionsIDs[0] != targetID {
		t.Errorf("revokeAllUserSessionsIDs = %v, want [%s]", tx.revokeAllUserSessionsIDs, targetID)
	}
	if len(auth.banCalls) != 1 || auth.banCalls[0] != targetID {
		t.Errorf("redis Ban calls = %v, want [%s]", auth.banCalls, targetID)
	}
	if len(spy.auditCalls) != 1 || spy.auditCalls[0].Action != auditlog.ActionAdminUserBanned {
		t.Fatalf("auditCalls = %+v, want one admin.user.banned entry", spy.auditCalls)
	}
}

func TestSetBan_BanRedisFailureIsBestEffort(t *testing.T) {
	// D3: the DB column is the source of truth. A failure priming the
	// Redis ban cache must not fail the whole ban — it self-heals on the
	// target's next login attempt (Task 1.5's Login always re-primes when
	// BannedAt is set, regardless of what's already cached).
	adminID, targetID := uuid.New(), uuid.New()
	var tx *mockAdminTxQuerier
	store := &mockAdminStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			if id == adminID {
				return db.User{ID: adminID, PasswordHash: adminBcryptHash}, nil
			}
			return db.User{ID: targetID}, nil
		},
		withTx: withMockAdminTx(&tx),
	}
	auth := alwaysAllowReauth()
	auth.banErr = errors.New("redis unavailable")
	svc := NewService(store, &mockCountCache{}, newTestAudit(&spyQuerier{}), nil, auth, nil, nil, slog.New(slog.NewTextHandler(os.Stdout, nil)))

	if err := svc.SetBan(context.Background(), AdminContext{AdminID: adminID}, targetID, true, nil, "password123"); err != nil {
		t.Fatalf("SetBan() error = %v, want nil (a Redis ban-cache failure must not fail the request)", err)
	}
}

func TestSetBan_UnbanRedisFailurePropagates(t *testing.T) {
	// The inverse of the above: banned:<userId> carries no TTL, so a
	// failed Unban has no self-healing path anywhere else in the system.
	// This one must surface as a real error.
	adminID, targetID := uuid.New(), uuid.New()
	var tx *mockAdminTxQuerier
	store := &mockAdminStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			if id == adminID {
				return db.User{ID: adminID, PasswordHash: adminBcryptHash}, nil
			}
			return db.User{ID: targetID}, nil
		},
		withTx: withMockAdminTx(&tx),
	}
	auth := alwaysAllowReauth()
	auth.unbanErr = errors.New("redis unavailable")
	svc := NewService(store, &mockCountCache{}, newTestAudit(&spyQuerier{}), nil, auth, nil, nil, slog.New(slog.NewTextHandler(os.Stdout, nil)))

	err := svc.SetBan(context.Background(), AdminContext{AdminID: adminID}, targetID, false, nil, "password123")
	if !errors.Is(err, auth.unbanErr) {
		t.Fatalf("SetBan() error = %v, want the redis Unban error to propagate", err)
	}
	// The DB write itself must still have gone through — an unban is not
	// held back by the cache-clear step failing after it.
	if len(tx.setUserBanCalls) != 1 || tx.setUserBanCalls[0].BannedAt.Valid {
		t.Errorf("setUserBanCalls = %+v, want one unban (BannedAt not valid)", tx.setUserBanCalls)
	}
}

// ---- DeleteOrganization ----

func TestDeleteOrganization_ConfirmMismatch(t *testing.T) {
	adminID, orgID := uuid.New(), uuid.New()
	store := &mockAdminStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{ID: adminID, PasswordHash: adminBcryptHash}, nil
		},
		adminGetOrganizationByID: func(ctx context.Context, id uuid.UUID) (db.Organization, error) {
			return db.Organization{ID: orgID, Slug: "acme-corp"}, nil
		},
		// adminDeleteOrganization intentionally left at its zero value's
		// no-op default is fine here since adminDeleteOrgCalls is what's
		// asserted, not a custom func.
	}
	svc := NewService(store, &mockCountCache{}, nil, nil, alwaysAllowReauth(), nil, nil, nil)

	err := svc.DeleteOrganization(context.Background(), AdminContext{AdminID: adminID}, orgID, "wrong-slug", "password123")
	if appErrorCode(t, err) != apperror.OrgConfirmMismatch {
		t.Fatalf("DeleteOrganization() error = %v, want OrgConfirmMismatch", err)
	}
	if store.adminDeleteOrgCalls != 0 {
		t.Errorf("AdminDeleteOrganization called %d times, want 0 on a confirm mismatch", store.adminDeleteOrgCalls)
	}
}

func TestDeleteOrganization_HappyPath_AuditsBeforeDeleting(t *testing.T) {
	adminID, orgID := uuid.New(), uuid.New()
	spy := &spyQuerier{}
	store := &mockAdminStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{ID: adminID, PasswordHash: adminBcryptHash}, nil
		},
		adminGetOrganizationByID: func(ctx context.Context, id uuid.UUID) (db.Organization, error) {
			return db.Organization{ID: orgID, Slug: "acme-corp", Name: "Acme Corp"}, nil
		},
		adminDeleteOrganization: func(ctx context.Context, id uuid.UUID) error {
			// The audit write must already be visible by the time the
			// delete itself runs — this is Task 3.1's documented
			// asymmetry, exercised directly rather than trusted by
			// reading the source.
			if len(spy.auditCalls) != 1 {
				t.Errorf("delete ran with %d audit calls recorded, want 1 (audit-before-delete)", len(spy.auditCalls))
			}
			return nil
		},
	}
	svc := NewService(store, &mockCountCache{}, newTestAudit(spy), nil, alwaysAllowReauth(), nil, nil, slog.New(slog.NewTextHandler(os.Stdout, nil)))

	if err := svc.DeleteOrganization(context.Background(), AdminContext{AdminID: adminID}, orgID, "acme-corp", "password123"); err != nil {
		t.Fatalf("DeleteOrganization() unexpected error: %v", err)
	}
	if store.adminDeleteOrgCalls != 1 {
		t.Errorf("AdminDeleteOrganization called %d times, want 1", store.adminDeleteOrgCalls)
	}
	if len(spy.auditCalls) != 1 || spy.auditCalls[0].Action != auditlog.ActionAdminOrgDeleted {
		t.Fatalf("auditCalls = %+v, want one admin.org.deleted entry", spy.auditCalls)
	}
}

// ---- Plan CRUD ----

func TestDeletePlan_PlanInUse(t *testing.T) {
	planID := uuid.New()
	store := &mockAdminStore{
		adminGetPlanByID: func(ctx context.Context, id uuid.UUID) (db.Plan, error) {
			return db.Plan{ID: planID, Name: "pro"}, nil
		},
		adminCountSubscriptionsByPlan: func(ctx context.Context, id uuid.UUID) (int64, error) {
			return 3, nil
		},
		// adminDeletePlan left at its zero-value no-op default; the
		// assertion below is on adminDeletePlanCalls, not on a rejection
		// from a custom func.
	}
	svc := NewService(store, &mockCountCache{}, nil, nil, nil, nil, nil, nil)

	err := svc.DeletePlan(context.Background(), AdminContext{AdminID: uuid.New()}, planID)
	if appErrorCode(t, err) != apperror.PlanInUse {
		t.Fatalf("DeletePlan() error = %v, want PlanInUse", err)
	}
	if store.adminDeletePlanCalls != 0 {
		t.Errorf("AdminDeletePlan called %d times, want 0 when the plan is in use", store.adminDeletePlanCalls)
	}
}

func TestDeletePlan_NotFound(t *testing.T) {
	store := &mockAdminStore{
		adminGetPlanByID: func(ctx context.Context, id uuid.UUID) (db.Plan, error) {
			return db.Plan{}, pgx.ErrNoRows
		},
	}
	svc := NewService(store, &mockCountCache{}, nil, nil, nil, nil, nil, nil)

	err := svc.DeletePlan(context.Background(), AdminContext{AdminID: uuid.New()}, uuid.New())
	if appErrorCode(t, err) != apperror.NotFound {
		t.Fatalf("DeletePlan() error = %v, want NotFound", err)
	}
}

// ---- Impersonation (execution plan Phase 4) ----

// stubSigner records what it was asked to sign so the guard-ordering tests
// below can assert a token was NOT minted on a refusal path.
type stubSigner struct {
	calls  int
	target uuid.UUID
	actor  uuid.UUID
	ttl    time.Duration
	err    error
}

func (s *stubSigner) SignImpersonationToken(targetID uuid.UUID, targetEmail string, actorID uuid.UUID, ttl time.Duration) (string, error) {
	s.calls++
	s.target, s.actor, s.ttl = targetID, actorID, ttl
	return "signed.impersonation.token", s.err
}

var _ impersonationSigner = (*stubSigner)(nil)

func TestImpersonate_HappyPath(t *testing.T) {
	actorID, targetID := uuid.New(), uuid.New()
	name := "Tenant User"
	signer := &stubSigner{}
	spy := &spyQuerier{}

	svc := NewService(&mockAdminStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{ID: targetID, Email: "tenant@example.com", DisplayName: &name}, nil
		},
	}, &mockCountCache{}, newTestAudit(spy), nil, nil, signer, nil, slog.New(slog.NewTextHandler(os.Stdout, nil)))

	got, err := svc.Impersonate(context.Background(),
		AdminContext{AdminID: actorID, IP: "10.0.0.1", UserAgent: "console/1.0"},
		targetID, "investigating a 401 from their MCP client")
	if err != nil {
		t.Fatalf("Impersonate: %v", err)
	}

	if got.AccessToken != "signed.impersonation.token" {
		t.Errorf("AccessToken = %q, want the signer's output", got.AccessToken)
	}
	if got.ExpiresIn != 600 {
		t.Errorf("ExpiresIn = %d, want 600 (the 10-minute TTL in seconds)", got.ExpiresIn)
	}
	if got.User.ID != targetID || got.User.Email != "tenant@example.com" {
		t.Errorf("User = %+v, want the target's identity", got.User)
	}
	// sub is the target and act is the staff member, never the reverse.
	if signer.target != targetID || signer.actor != actorID {
		t.Errorf("signed target/actor = %v/%v, want %v/%v", signer.target, signer.actor, targetID, actorID)
	}
	if signer.ttl != impersonationTTL {
		t.Errorf("signed ttl = %v, want %v", signer.ttl, impersonationTTL)
	}
	if len(spy.auditCalls) != 1 || spy.auditCalls[0].Action != auditlog.ActionAdminImpersonationStarted {
		t.Fatalf("auditCalls = %+v, want one admin.impersonation.started entry", spy.auditCalls)
	}
	// The reason is the entire accountability story for the next 10
	// minutes, so it has to actually reach the audit row.
	if !strings.Contains(string(spy.auditCalls[0].Metadata), "investigating a 401") {
		t.Errorf("audit metadata = %s, want it to carry the reason", spy.auditCalls[0].Metadata)
	}
}

// Each refusal must happen BEFORE a token is minted — a returned error with
// a signed token already handed out would be the whole control failing.
func TestImpersonate_RefusalsMintNoToken(t *testing.T) {
	actorID, targetID := uuid.New(), uuid.New()
	role := "support"

	tests := []struct {
		name     string
		user     db.User
		userErr  error
		wantCode string
	}{
		{
			name:     "unknown user",
			userErr:  pgx.ErrNoRows,
			wantCode: apperror.UserNotFound,
		},
		{
			name:     "platform staff cannot be impersonated",
			user:     db.User{ID: targetID, Email: "staff@example.com", PlatformRole: &role},
			wantCode: apperror.CannotImpersonateStaff,
		},
		{
			name:     "banned user cannot be impersonated",
			user:     db.User{ID: targetID, Email: "banned@example.com", BannedAt: pgtype.Timestamp{Time: time.Now(), Valid: true}},
			wantCode: apperror.AccountSuspended,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			signer := &stubSigner{}
			svc := NewService(&mockAdminStore{
				getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
					return tt.user, tt.userErr
				},
			}, &mockCountCache{}, nil, nil, nil, signer, nil, nil)

			_, err := svc.Impersonate(context.Background(),
				AdminContext{AdminID: actorID}, targetID, "a sufficiently long reason")
			if appErrorCode(t, err) != tt.wantCode {
				t.Fatalf("Impersonate() error = %v, want %s", err, tt.wantCode)
			}
			if signer.calls != 0 {
				t.Errorf("signer called %d times on a refusal; a refused impersonation must mint nothing", signer.calls)
			}
		})
	}
}

// A banned staff account reports the staff refusal, not the ban: demoting
// first is the action the caller has to take either way, and it avoids
// leaking one account's ban state through the other refusal.
func TestImpersonate_StaffCheckedBeforeBan(t *testing.T) {
	role := "superadmin"
	signer := &stubSigner{}
	svc := NewService(&mockAdminStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{
				ID:           uuid.New(),
				Email:        "banned-staff@example.com",
				PlatformRole: &role,
				BannedAt:     pgtype.Timestamp{Time: time.Now(), Valid: true},
			}, nil
		},
	}, &mockCountCache{}, nil, nil, nil, signer, nil, nil)

	_, err := svc.Impersonate(context.Background(),
		AdminContext{AdminID: uuid.New()}, uuid.New(), "a sufficiently long reason")
	if appErrorCode(t, err) != apperror.CannotImpersonateStaff {
		t.Fatalf("error = %v, want CANNOT_IMPERSONATE_STAFF for a banned staff account", err)
	}
}
