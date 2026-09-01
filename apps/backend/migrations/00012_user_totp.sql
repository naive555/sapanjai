-- +goose Up
CREATE TABLE "user_totp" (
  "user_id" uuid PRIMARY KEY REFERENCES "users"("id") ON DELETE cascade,
  "secret_encrypted" jsonb NOT NULL,
  "recovery_codes" text[] NOT NULL,
  "confirmed_at" timestamp,
  "created_at" timestamp DEFAULT now() NOT NULL
);

-- +goose Down
DROP TABLE "user_totp";
