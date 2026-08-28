package admin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sapanjai/backend/internal/infra/database/db"
)

// mockAdminStore embeds the (nil) adminStore interface so any method this
// test doesn't override panics loudly if accidentally called, rather than
// silently returning a zero value — the same "hand-mock the narrow
// interface" pattern mcpkey/connector's own service tests use, adapted for
// an interface this wide.
type mockAdminStore struct {
	adminStore

	getUserByID func(ctx context.Context, id uuid.UUID) (db.User, error)
}

func (m *mockAdminStore) GetUserByID(ctx context.Context, id uuid.UUID) (db.User, error) {
	return m.getUserByID(ctx, id)
}

var _ adminStore = (*mockAdminStore)(nil)

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
	}, &mockCountCache{}, nil, nil, nil)

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
	}, &mockCountCache{}, nil, nil, nil)

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
	}, &mockCountCache{}, nil, nil, nil)

	_, err := svc.Me(context.Background(), uuid.New())
	if !errors.Is(err, dbErr) {
		t.Fatalf("expected the raw db error to propagate, got %v", err)
	}
}

// ---- cachedCount ----

func TestCachedCount_MissComputesAndCaches(t *testing.T) {
	cache := &mockCountCache{}
	svc := NewService(nil, cache, nil, nil, nil)

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
	svc := NewService(nil, cache, nil, nil, nil)

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
	svc := NewService(nil, cache, nil, nil, nil)

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
	svc := NewService(nil, cache, nil, nil, nil)

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
	svc := NewService(nil, &mockCountCache{}, nil, nil, nil)

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
