-- Vendored from oramasearch/dashboard .drizzle/0008_bouncy_ravenous.sql (control-plane Postgres
-- schema, operator#378). Source: branch cloud-migration
-- (https://github.com/oramasearch/dashboard/pull/112, unmerged; main and dev stop at 0007),
-- commit 8e8f9a661f8f23461c5e5a2ded94e9d84e0a67b6. The bytes after the marker line below are
-- that file unchanged (sha256 7df81388abc29d8a898dc941a02dc0e0d5065c2a8a615032cc0bbda66454ced1).
-- Applied by the operator's <cr>-controldb-schema-0008 PostgresSchema after <cr>-controldb-schema
-- (0000..0007) is applied, guarded on team_migrations. DO NOT hand-edit.

-- ============ 0008_bouncy_ravenous.sql ============
-- Creates the objects the Drizzle schema defines that 0000-0007 never created
-- (production received them through `drizzle-kit push`), plus team_migrations.
-- Every statement is guarded so this applies cleanly to a database that already has them.
DO $$ BEGIN
 CREATE TYPE "public"."error_type" AS ENUM('documents_limit', 'ai_sessions_limit');
EXCEPTION
 WHEN duplicate_object THEN null;
END $$;
--> statement-breakpoint
DO $$ BEGIN
 CREATE TYPE "public"."llm_group" AS ENUM('base', 'pro', 'premium');
EXCEPTION
 WHEN duplicate_object THEN null;
END $$;
--> statement-breakpoint
CREATE TABLE IF NOT EXISTS "limits_errors" (
	"id" uuid PRIMARY KEY NOT NULL,
	"project_id" uuid NOT NULL,
	"index_id" uuid NOT NULL,
	"error_type" "error_type" NOT NULL,
	"error_message" varchar(255) NOT NULL,
	"created_by" uuid NOT NULL,
	"created_at" timestamp DEFAULT now() NOT NULL,
	"updated_at" timestamp,
	"deleted_at" timestamp
);
--> statement-breakpoint
CREATE TABLE IF NOT EXISTS "llms" (
	"id" uuid PRIMARY KEY NOT NULL,
	"position" integer DEFAULT 100 NOT NULL,
	"name" varchar(120) NOT NULL,
	"provider" varchar(120) NOT NULL,
	"model_call_name" varchar(120) NOT NULL,
	"group" "llm_group" DEFAULT 'base' NOT NULL,
	"available" boolean DEFAULT true NOT NULL,
	"max_input_tokens" integer NOT NULL,
	"max_output_tokens" integer NOT NULL,
	"created_at" timestamp DEFAULT now() NOT NULL,
	"updated_at" timestamp,
	"deleted_at" timestamp
);
--> statement-breakpoint
CREATE TABLE IF NOT EXISTS "pinning_rules_map" (
	"id" uuid PRIMARY KEY NOT NULL,
	"project_id" uuid NOT NULL,
	"rule_ids" text NOT NULL,
	"created_by" uuid NOT NULL,
	"created_at" timestamp DEFAULT now() NOT NULL,
	"updated_at" timestamp,
	"deleted_at" timestamp
);
--> statement-breakpoint
CREATE TABLE IF NOT EXISTS "subscription_plans" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"name" varchar(255) NOT NULL,
	"description" varchar(1000),
	"price_monthly_cents" integer NOT NULL,
	"price_yearly_cents" integer NOT NULL,
	"stripe_price_id_monthly" varchar(255),
	"stripe_price_id_yearly" varchar(255),
	"is_active" boolean DEFAULT true NOT NULL,
	"display_order" integer DEFAULT 0 NOT NULL,
	"search_fulltext_monthly_limit" integer DEFAULT -1 NOT NULL,
	"search_vector_monthly_limit" integer DEFAULT -1 NOT NULL,
	"search_hybrid_monthly_limit" integer DEFAULT -1 NOT NULL,
	"search_nlp_monthly_limit" integer DEFAULT -1 NOT NULL,
	"search_auto_monthly_limit" integer DEFAULT -1 NOT NULL,
	"ai_sessions_included_monthly" integer,
	"cost_per_100_ai_sessions_base_cents" integer,
	"cost_per_100_ai_sessions_pro_cents" integer,
	"cost_per_100_ai_sessions_premium_cents" integer,
	"mcp_server_included" boolean DEFAULT true NOT NULL,
	"merchandising_included" boolean DEFAULT true NOT NULL,
	"synonyms_included" boolean DEFAULT true NOT NULL,
	"maximum_synonyms_per_project" integer DEFAULT -1 NOT NULL,
	"maximum_ai_synonyms_per_project" integer DEFAULT 5 NOT NULL,
	"training_shots_per_project" integer DEFAULT 9 NOT NULL,
	"cost_per_training_shot_upgrade_cents" integer,
	"teams_included" boolean DEFAULT false NOT NULL,
	"cost_per_team_cents" integer,
	"projects_included" integer DEFAULT 1 NOT NULL,
	"cost_per_additional_project_cents" integer,
	"indexes_per_project" integer DEFAULT 2 NOT NULL,
	"cost_per_10_indexes_cents" integer,
	"documents_included_per_project" integer DEFAULT 1000 NOT NULL,
	"maximum_documents_per_project" integer DEFAULT -1 NOT NULL,
	"cost_per_1000_documents_cents" integer,
	"analytics_window_days" integer DEFAULT 60 NOT NULL,
	"full_analytics_included" boolean DEFAULT false NOT NULL,
	"cost_per_full_analytics_upgrade_cents" integer,
	"cost_per_raw_analytics_cents" integer,
	"support_level_community" boolean DEFAULT true NOT NULL,
	"support_level_mail" boolean DEFAULT false NOT NULL,
	"support_level_slack" boolean DEFAULT false NOT NULL,
	"support_level_phone" boolean DEFAULT false NOT NULL,
	"support_sla_hours" integer,
	"customer_success_manager_included" boolean DEFAULT false NOT NULL,
	"jwt_authentication_enabled" boolean DEFAULT false NOT NULL,
	"on_prem_support" boolean DEFAULT false NOT NULL,
	"multi_cloud_support" boolean DEFAULT false NOT NULL,
	"included_regions" integer DEFAULT 1 NOT NULL,
	"cost_per_5_regions_upgrade_cents" integer,
	"cost_per_federated_node_cents" integer,
	"attribution_required" boolean DEFAULT true NOT NULL,
	"created_at" timestamp DEFAULT now() NOT NULL,
	"updated_at" timestamp,
	"deleted_at" timestamp
);
--> statement-breakpoint
CREATE TABLE IF NOT EXISTS "stripe_addon_prices" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"plan_id" uuid NOT NULL,
	"addon_type" varchar(50) NOT NULL,
	"billing_period" varchar(20) NOT NULL,
	"stripe_price_id" varchar(255) NOT NULL,
	"created_at" timestamp DEFAULT now() NOT NULL,
	"updated_at" timestamp,
	"deleted_at" timestamp,
	CONSTRAINT "stripe_addon_prices_stripe_price_id_unique" UNIQUE("stripe_price_id")
);
--> statement-breakpoint
CREATE TABLE IF NOT EXISTS "subscription_items" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"team_id" uuid NOT NULL,
	"project_id" uuid,
	"addon_type" varchar(50) NOT NULL,
	"quantity" integer DEFAULT 1 NOT NULL,
	"unit_price_cents" integer NOT NULL,
	"billing_type" varchar(20) DEFAULT 'recurring' NOT NULL,
	"stripe_price_id" varchar(255),
	"stripe_subscription_item_id" varchar(255),
	"is_active" boolean DEFAULT true NOT NULL,
	"active_from" timestamp with time zone DEFAULT now() NOT NULL,
	"active_until" timestamp with time zone,
	"created_at" timestamp DEFAULT now() NOT NULL,
	"updated_at" timestamp,
	"deleted_at" timestamp,
	CONSTRAINT "subscription_items_stripe_subscription_item_id_unique" UNIQUE("stripe_subscription_item_id")
);
--> statement-breakpoint
CREATE TABLE IF NOT EXISTS "team_migrations" (
	"team_id" uuid PRIMARY KEY NOT NULL,
	"state" text NOT NULL,
	"target" text,
	"updated_at" timestamp with time zone DEFAULT now() NOT NULL,
	CONSTRAINT "team_migrations_state_check" CHECK ("team_migrations"."state" IN ('frozen', 'moved'))
);
--> statement-breakpoint
ALTER TABLE "teams" ADD COLUMN IF NOT EXISTS "subscription_plan_id" uuid DEFAULT '0d735744-e58e-4438-b618-97523e407cdc' NOT NULL;
--> statement-breakpoint
ALTER TABLE "teams" ADD COLUMN IF NOT EXISTS "stripe_customer_id" varchar(255);
--> statement-breakpoint
ALTER TABLE "teams" ADD COLUMN IF NOT EXISTS "stripe_subscription_id" varchar(255);
--> statement-breakpoint
ALTER TABLE "teams" ADD COLUMN IF NOT EXISTS "has_payment_method" boolean DEFAULT false NOT NULL;
--> statement-breakpoint
DO $$ BEGIN
 ALTER TABLE "limits_errors" ADD CONSTRAINT "limits_errors_project_id_projects_id_fk" FOREIGN KEY ("project_id") REFERENCES "public"."projects"("id") ON DELETE cascade ON UPDATE no action;
EXCEPTION
 WHEN duplicate_object THEN null;
END $$;
--> statement-breakpoint
DO $$ BEGIN
 ALTER TABLE "limits_errors" ADD CONSTRAINT "limits_errors_index_id_indexes_id_fk" FOREIGN KEY ("index_id") REFERENCES "public"."indexes"("id") ON DELETE cascade ON UPDATE no action;
EXCEPTION
 WHEN duplicate_object THEN null;
END $$;
--> statement-breakpoint
DO $$ BEGIN
 ALTER TABLE "limits_errors" ADD CONSTRAINT "limits_errors_created_by_users_id_fk" FOREIGN KEY ("created_by") REFERENCES "public"."users"("id") ON DELETE no action ON UPDATE no action;
EXCEPTION
 WHEN duplicate_object THEN null;
END $$;
--> statement-breakpoint
DO $$ BEGIN
 ALTER TABLE "pinning_rules_map" ADD CONSTRAINT "pinning_rules_map_project_id_projects_id_fk" FOREIGN KEY ("project_id") REFERENCES "public"."projects"("id") ON DELETE cascade ON UPDATE no action;
EXCEPTION
 WHEN duplicate_object THEN null;
END $$;
--> statement-breakpoint
DO $$ BEGIN
 ALTER TABLE "pinning_rules_map" ADD CONSTRAINT "pinning_rules_map_created_by_users_id_fk" FOREIGN KEY ("created_by") REFERENCES "public"."users"("id") ON DELETE no action ON UPDATE no action;
EXCEPTION
 WHEN duplicate_object THEN null;
END $$;
--> statement-breakpoint
DO $$ BEGIN
 ALTER TABLE "stripe_addon_prices" ADD CONSTRAINT "stripe_addon_prices_plan_id_subscription_plans_id_fk" FOREIGN KEY ("plan_id") REFERENCES "public"."subscription_plans"("id") ON DELETE cascade ON UPDATE no action;
EXCEPTION
 WHEN duplicate_object THEN null;
END $$;
--> statement-breakpoint
DO $$ BEGIN
 ALTER TABLE "subscription_items" ADD CONSTRAINT "subscription_items_team_id_teams_id_fk" FOREIGN KEY ("team_id") REFERENCES "public"."teams"("id") ON DELETE cascade ON UPDATE no action;
EXCEPTION
 WHEN duplicate_object THEN null;
END $$;
--> statement-breakpoint
DO $$ BEGIN
 ALTER TABLE "subscription_items" ADD CONSTRAINT "subscription_items_project_id_projects_id_fk" FOREIGN KEY ("project_id") REFERENCES "public"."projects"("id") ON DELETE cascade ON UPDATE no action;
EXCEPTION
 WHEN duplicate_object THEN null;
END $$;
--> statement-breakpoint
DO $$ BEGIN
 ALTER TABLE "team_migrations" ADD CONSTRAINT "team_migrations_team_id_teams_id_fk" FOREIGN KEY ("team_id") REFERENCES "public"."teams"("id") ON DELETE no action ON UPDATE no action;
EXCEPTION
 WHEN duplicate_object THEN null;
END $$;
--> statement-breakpoint
DO $$ BEGIN
 ALTER TABLE "teams" ADD CONSTRAINT "teams_subscription_plan_id_subscription_plans_id_fk" FOREIGN KEY ("subscription_plan_id") REFERENCES "public"."subscription_plans"("id") ON DELETE no action ON UPDATE no action;
EXCEPTION
 WHEN duplicate_object THEN null;
END $$;
--> statement-breakpoint
DO $$ BEGIN
 ALTER TABLE "teams" ADD CONSTRAINT "teams_stripe_customer_id_unique" UNIQUE("stripe_customer_id");
EXCEPTION
 WHEN duplicate_object OR duplicate_table THEN null;
END $$;
--> statement-breakpoint
DO $$ BEGIN
 ALTER TABLE "teams" ADD CONSTRAINT "teams_stripe_subscription_id_unique" UNIQUE("stripe_subscription_id");
EXCEPTION
 WHEN duplicate_object OR duplicate_table THEN null;
END $$;
