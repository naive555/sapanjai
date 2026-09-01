-- name: UpsertUserTOTPSecret :exec
-- Backs POST /admin/2fa/enroll (execution plan Task 6.3). Re-callable: a
-- staff member who lost their authenticator app before confirming (or
-- wants to re-enroll a new one) can call enroll again, which wipes any
-- prior confirmation and recovery codes along with the old secret -- an
-- unconfirmed or replaced secret must never leave a stale confirmed_at or
-- a set of recovery codes tied to a key that no longer exists.
-- recovery_codes is NOT NULL with no column default (migration 00012), so
-- every insert must supply '{}' explicitly here; confirm sets the real ten.
INSERT INTO user_totp (user_id, secret_encrypted, recovery_codes)
VALUES ($1, $2, '{}')
ON CONFLICT (user_id) DO UPDATE
  SET secret_encrypted = $2, recovery_codes = '{}', confirmed_at = NULL;

-- name: GetUserTOTP :one
SELECT * FROM user_totp WHERE user_id = $1;

-- name: ConfirmUserTOTP :exec
-- Backs POST /admin/2fa/confirm: stamps confirmed_at and stores the ten
-- recovery-code hashes generated at confirm time (never at enroll time,
-- since enroll may be called repeatedly before a confirm ever lands).
UPDATE user_totp SET confirmed_at = now(), recovery_codes = $2 WHERE user_id = $1;

-- name: UpdateUserTOTPRecoveryCodes :exec
-- Backs POST /admin/2fa/verify's recovery-code path: persists the
-- caller-supplied remaining set after one hash is removed, so a recovery
-- code is usable exactly once.
UPDATE user_totp SET recovery_codes = $2 WHERE user_id = $1;
