package admin

import (
	"context"

	"github.com/google/uuid"

	"github.com/sapanjai/backend/internal/infra/database/db"
)

// mockAdminTxQuerier stands in for the db.Querier a Phase 3 mutation's
// WithTx body receives, mirroring internal/module/auth/tx_test.go's
// mockTxQuerier exactly: db.Querier embedded (left nil) and overridden only
// for the statements ChangePlatformRole/SetBan actually issue
// (SetUserPlatformRole, SetUserBan, RevokeAllUserSessions). Any other
// query reached from inside a transaction body hits the nil embedded
// interface and panics, naming the method, instead of silently returning a
// zero value.
type mockAdminTxQuerier struct {
	db.Querier

	setUserPlatformRoleCalls []db.SetUserPlatformRoleParams
	setUserBanCalls          []db.SetUserBanParams
	revokeAllUserSessionsIDs []uuid.UUID
	setUserPlatformRoleErr   error
	setUserBanErr            error
	revokeAllUserSessionsErr error
}

func (m *mockAdminTxQuerier) SetUserPlatformRole(ctx context.Context, arg db.SetUserPlatformRoleParams) error {
	m.setUserPlatformRoleCalls = append(m.setUserPlatformRoleCalls, arg)
	return m.setUserPlatformRoleErr
}

func (m *mockAdminTxQuerier) SetUserBan(ctx context.Context, arg db.SetUserBanParams) error {
	m.setUserBanCalls = append(m.setUserBanCalls, arg)
	return m.setUserBanErr
}

func (m *mockAdminTxQuerier) RevokeAllUserSessions(ctx context.Context, userID uuid.UUID) error {
	m.revokeAllUserSessionsIDs = append(m.revokeAllUserSessionsIDs, userID)
	return m.revokeAllUserSessionsErr
}

// withMockAdminTx returns an adminStore.WithTx implementation that runs fn
// against a fresh mockAdminTxQuerier and hands it back through out, so a
// test can assert on what the transaction body did.
func withMockAdminTx(out **mockAdminTxQuerier) func(ctx context.Context, fn func(q db.Querier) error) error {
	return func(ctx context.Context, fn func(q db.Querier) error) error {
		q := &mockAdminTxQuerier{}
		*out = q
		return fn(q)
	}
}
