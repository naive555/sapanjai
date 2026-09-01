package main

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sapanjai/backend/internal/infra/database/db"
)

// ---- hand-mocked grantStore ----

type mockGrantStore struct {
	getUserByEmail   func(ctx context.Context, email string) (db.User, error)
	setPlatformRole  func(ctx context.Context, arg db.SetUserPlatformRoleParams) error
	setRoleCallCount int
	lastSetArg       db.SetUserPlatformRoleParams
}

func (m *mockGrantStore) GetUserByEmail(ctx context.Context, email string) (db.User, error) {
	return m.getUserByEmail(ctx, email)
}

func (m *mockGrantStore) SetUserPlatformRole(ctx context.Context, arg db.SetUserPlatformRoleParams) error {
	m.setRoleCallCount++
	m.lastSetArg = arg
	if m.setPlatformRole != nil {
		return m.setPlatformRole(ctx, arg)
	}
	return nil
}

func TestGrantPlatformRole_UnknownEmailRefused(t *testing.T) {
	store := &mockGrantStore{
		getUserByEmail: func(ctx context.Context, email string) (db.User, error) {
			return db.User{}, pgx.ErrNoRows
		},
	}

	err := grantPlatformRole(context.Background(), store, "nobody@example.com", "superadmin")
	if !errors.Is(err, errUnknownEmail) {
		t.Fatalf("err = %v, want errUnknownEmail", err)
	}
	if store.setRoleCallCount != 0 {
		t.Errorf("SetUserPlatformRole called %d times, want 0 for an unknown email", store.setRoleCallCount)
	}
}

func TestGrantPlatformRole_LookupErrorPropagates(t *testing.T) {
	wantErr := errors.New("connection reset")
	store := &mockGrantStore{
		getUserByEmail: func(ctx context.Context, email string) (db.User, error) {
			return db.User{}, wantErr
		},
	}

	err := grantPlatformRole(context.Background(), store, "user@example.com", "superadmin")
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want it to wrap %v", err, wantErr)
	}
	if errors.Is(err, errUnknownEmail) {
		t.Error("a real infra error must not be reported as errUnknownEmail")
	}
}

func TestGrantPlatformRole_GrantsRole(t *testing.T) {
	userID := uuid.New()
	store := &mockGrantStore{
		getUserByEmail: func(ctx context.Context, email string) (db.User, error) {
			return db.User{ID: userID, Email: email}, nil
		},
	}

	if err := grantPlatformRole(context.Background(), store, "user@example.com", "superadmin"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.setRoleCallCount != 1 {
		t.Fatalf("SetUserPlatformRole called %d times, want 1", store.setRoleCallCount)
	}
	if store.lastSetArg.ID != userID {
		t.Errorf("SetUserPlatformRole id = %v, want %v", store.lastSetArg.ID, userID)
	}
	if store.lastSetArg.PlatformRole == nil || *store.lastSetArg.PlatformRole != "superadmin" {
		t.Errorf("PlatformRole = %v, want \"superadmin\"", store.lastSetArg.PlatformRole)
	}
}

func TestGrantPlatformRole_NoneRevokesToNull(t *testing.T) {
	userID := uuid.New()
	store := &mockGrantStore{
		getUserByEmail: func(ctx context.Context, email string) (db.User, error) {
			return db.User{ID: userID, Email: email}, nil
		},
	}

	if err := grantPlatformRole(context.Background(), store, "user@example.com", "none"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.lastSetArg.PlatformRole != nil {
		t.Errorf("PlatformRole = %v, want nil for -role none", *store.lastSetArg.PlatformRole)
	}
}

func TestGrantPlatformRole_SetRoleErrorPropagates(t *testing.T) {
	wantErr := errors.New("constraint violation")
	store := &mockGrantStore{
		getUserByEmail: func(ctx context.Context, email string) (db.User, error) {
			return db.User{ID: uuid.New(), Email: email}, nil
		},
		setPlatformRole: func(ctx context.Context, arg db.SetUserPlatformRoleParams) error {
			return wantErr
		},
	}

	err := grantPlatformRole(context.Background(), store, "user@example.com", "support")
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want it to wrap %v", err, wantErr)
	}
}
