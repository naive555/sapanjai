-- +goose Up
-- Step 1 of docs/12-billing-and-metering.md (not yet written; see
-- .claude/plans/2026-09-13-billing-and-usage-metering.md): pricing +
-- Stripe linkage. Entitlements keep resolving from plans.limits /
-- org_subscriptions.custom_limits alone (subscription.Service.EffectiveLimits)
-- — nothing here changes that column or its semantics; Stripe only gets a
-- place to be recorded.
ALTER TABLE "plans" ADD COLUMN "stripe_product_id" text;
ALTER TABLE "plans" ADD COLUMN "is_public" boolean NOT NULL DEFAULT true;
ALTER TABLE "plans" ADD COLUMN "sort_order" integer NOT NULL DEFAULT 0;

-- One Stripe Product per plan (stripe_product_id above), many Prices under
-- it — monthly/annual/multi-currency are rows here, not columns on plans,
-- so every invoice line item for a plan reads the same product name
-- regardless of interval or currency.
CREATE TABLE "plan_prices" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"plan_id" uuid NOT NULL,
	"stripe_price_id" text NOT NULL,
	"unit_amount" bigint NOT NULL,
	"currency" text NOT NULL,
	"interval" text NOT NULL,
	"active" boolean NOT NULL DEFAULT true,
	"created_at" timestamp DEFAULT now() NOT NULL,
	CONSTRAINT "plan_prices_stripe_price_id_unique" UNIQUE("stripe_price_id"),
	-- Mirrors the users_platform_role_check style (migration 00011): a
	-- fixed enum enforced in the database, not just application code.
	-- "interval" is a reserved-ish word in Postgres and in the generated
	-- Go struct field, so it stays quoted everywhere it appears.
	CONSTRAINT "plan_prices_interval_check" CHECK ("interval" IN ('month', 'year'))
);
ALTER TABLE "plan_prices" ADD CONSTRAINT "plan_prices_plan_id_plans_id_fk" FOREIGN KEY ("plan_id") REFERENCES "public"."plans"("id") ON DELETE cascade ON UPDATE no action;
-- The only access pattern today: render one plan's prices.
CREATE INDEX IF NOT EXISTS "idx_plan_prices_plan_id" ON "plan_prices" ("plan_id");

-- stripe_customer_id/stripe_subscription_id/status/current_period_end stay
-- nullable by design (decision 4, plan §Decisions): stripe_subscription_id
-- IS NULL is the canonical "not paying" signal, and a Customer can exist
-- (lazily created on first billing interaction) before any Subscription
-- does. Do not backfill or add NOT NULL here.
ALTER TABLE "org_subscriptions" ADD COLUMN "stripe_customer_id" text;
ALTER TABLE "org_subscriptions" ADD COLUMN "stripe_subscription_id" text;
ALTER TABLE "org_subscriptions" ADD COLUMN "status" text;
ALTER TABLE "org_subscriptions" ADD COLUMN "current_period_end" timestamp;
ALTER TABLE "org_subscriptions" ADD COLUMN "cancel_at_period_end" boolean NOT NULL DEFAULT false;

-- Partial (not full) unique: most rows have both columns NULL (free orgs,
-- decision 4), and a plain UNIQUE would allow at most one such row. Each
-- non-null Stripe id still identifies exactly one org, which the webhook
-- (step 7) relies on when it looks up an org_subscriptions row by
-- customer or subscription id off an incoming event.
CREATE UNIQUE INDEX IF NOT EXISTS "idx_org_subscriptions_stripe_customer_id" ON "org_subscriptions" ("stripe_customer_id") WHERE "stripe_customer_id" IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS "idx_org_subscriptions_stripe_subscription_id" ON "org_subscriptions" ("stripe_subscription_id") WHERE "stripe_subscription_id" IS NOT NULL;

-- Webhook idempotency (step 7): Stripe retries and can deliver the same
-- event more than once. The event id is the primary key, so a second
-- delivery is a duplicate-key no-op rather than a second reconciliation.
CREATE TABLE "stripe_events" (
	"id" text PRIMARY KEY,
	"type" text NOT NULL,
	"received_at" timestamp DEFAULT now() NOT NULL
);

-- +goose Down
DROP TABLE "stripe_events";
DROP INDEX IF EXISTS "idx_org_subscriptions_stripe_subscription_id";
DROP INDEX IF EXISTS "idx_org_subscriptions_stripe_customer_id";
ALTER TABLE "org_subscriptions" DROP COLUMN "cancel_at_period_end";
ALTER TABLE "org_subscriptions" DROP COLUMN "current_period_end";
ALTER TABLE "org_subscriptions" DROP COLUMN "status";
ALTER TABLE "org_subscriptions" DROP COLUMN "stripe_subscription_id";
ALTER TABLE "org_subscriptions" DROP COLUMN "stripe_customer_id";
DROP INDEX IF EXISTS "idx_plan_prices_plan_id";
ALTER TABLE "plan_prices" DROP CONSTRAINT "plan_prices_plan_id_plans_id_fk";
DROP TABLE "plan_prices";
ALTER TABLE "plans" DROP COLUMN "sort_order";
ALTER TABLE "plans" DROP COLUMN "is_public";
ALTER TABLE "plans" DROP COLUMN "stripe_product_id";
