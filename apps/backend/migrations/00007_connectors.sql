-- +goose Up
CREATE TABLE "connectors" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"organization_id" uuid NOT NULL,
	"name" text NOT NULL,
	"type" text NOT NULL,
	"encrypted_config" jsonb NOT NULL,
	"status" text DEFAULT 'inactive' NOT NULL,
	"last_health_check_at" timestamp,
	"created_at" timestamp DEFAULT now() NOT NULL,
	"updated_at" timestamp DEFAULT now() NOT NULL,
	CONSTRAINT "connectors_organization_id_name_unique" UNIQUE("organization_id","name")
);
ALTER TABLE "connectors" ADD CONSTRAINT "connectors_organization_id_organizations_id_fk" FOREIGN KEY ("organization_id") REFERENCES "public"."organizations"("id") ON DELETE cascade ON UPDATE no action;
CREATE INDEX IF NOT EXISTS "idx_connectors_organization_id" ON "connectors" ("organization_id");

-- +goose Down
DROP TABLE "connectors";
