-- Vendored from oramasearch/dashboard .drizzle (control-plane Postgres schema, operator#34).
-- Applied by the operator's control-DB migration Job (idempotent). Keep in sync when the
-- dashboard schema changes (drizzle migrations 0000..0007, journal order). DO NOT hand-edit.

-- ============ 0000_perpetual_frank_castle.sql ============
CREATE TYPE "public"."invitation_status" AS ENUM('pending', 'accepted', 'declined', 'expired');--> statement-breakpoint
CREATE TYPE "public"."team_member_role" AS ENUM('owner', 'admin', 'member');--> statement-breakpoint
CREATE TYPE "public"."team_member_status" AS ENUM('active', 'pending', 'suspended');--> statement-breakpoint
CREATE TABLE "crawler_configs" (
	"id" uuid PRIMARY KEY NOT NULL,
	"index_id" uuid NOT NULL,
	"start_url" varchar(500) NOT NULL,
	"preset" varchar(50) DEFAULT 'DOCUMENTATION' NOT NULL,
	"ignore_patterns" jsonb DEFAULT '[]'::jsonb NOT NULL,
	"facet_position" integer DEFAULT 1 NOT NULL,
	"client_side_rendering" boolean DEFAULT true NOT NULL,
	"frequency" varchar(20) DEFAULT 'weekly' NOT NULL,
	"last_crawl_at" text,
	"next_crawl_at" text,
	"crawl_status" varchar(50) DEFAULT 'created' NOT NULL,
	"crawl_error" text,
	"created_at" timestamp DEFAULT now() NOT NULL,
	"updated_at" timestamp,
	"deleted_at" timestamp,
	CONSTRAINT "crawler_configs_id_unique" UNIQUE("id")
);
--> statement-breakpoint
CREATE TABLE "indexes" (
	"id" uuid PRIMARY KEY NOT NULL,
	"project_id" uuid NOT NULL,
	"name" varchar(120) NOT NULL,
	"description" varchar(255) NOT NULL,
	"is_airbyte" boolean DEFAULT false NOT NULL,
	"data_source_id" varchar(120) NOT NULL,
	"airbyte_connection_id" varchar(120),
	"created_by" uuid NOT NULL,
	"is_default" boolean DEFAULT false NOT NULL,
	"created_at" timestamp DEFAULT now() NOT NULL,
	"updated_at" timestamp,
	"deleted_at" timestamp,
	CONSTRAINT "indexes_id_unique" UNIQUE("id")
);
--> statement-breakpoint
CREATE TABLE "oramacore_clusters" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"name" varchar(255) NOT NULL,
	"writer_url" varchar(255) NOT NULL,
	"reader_url" varchar(255) NOT NULL,
	"cluster_reader_replica_count" integer NOT NULL,
	"cluster_writer_count" integer NOT NULL,
	"cluster_reader_regions" jsonb NOT NULL,
	"cluster_writer_regions" jsonb NOT NULL,
	"master_api_key" varchar(255) NOT NULL,
	"cluster_prefix" varchar(4),
	"created_at" timestamp DEFAULT now() NOT NULL,
	"updated_at" timestamp,
	"deleted_at" timestamp,
	CONSTRAINT "oramacore_clusters_id_unique" UNIQUE("id")
);
--> statement-breakpoint
CREATE TABLE "private_api_key" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"user_id" uuid NOT NULL,
	"private_api_key" varchar(45) NOT NULL,
	"project_id" uuid NOT NULL,
	"created_at" timestamp DEFAULT now() NOT NULL,
	"updated_at" timestamp,
	"deleted_at" timestamp,
	CONSTRAINT "private_api_key_private_api_key_unique" UNIQUE("private_api_key")
);
--> statement-breakpoint
CREATE TABLE "projects" (
	"id" uuid PRIMARY KEY NOT NULL,
	"name" varchar(120) NOT NULL,
	"read_api_key" varchar(64) NOT NULL,
	"write_api_key" varchar(64) NOT NULL,
	"oramacore_cluster_id" uuid NOT NULL,
	"created_by" uuid NOT NULL,
	"created_at" timestamp DEFAULT now() NOT NULL,
	"updated_at" timestamp,
	"deleted_at" timestamp,
	CONSTRAINT "projects_id_unique" UNIQUE("id")
);
--> statement-breakpoint
CREATE TABLE "team_invitations" (
	"id" uuid PRIMARY KEY NOT NULL,
	"team_id" uuid NOT NULL,
	"invited_email" varchar(100) NOT NULL,
	"invited_by" uuid NOT NULL,
	"invitation_token" varchar(100) NOT NULL,
	"status" "invitation_status" DEFAULT 'pending' NOT NULL,
	"expires_at" timestamp with time zone NOT NULL,
	"accepted_at" timestamp with time zone,
	"accepted_by" uuid,
	"created_at" timestamp DEFAULT now() NOT NULL,
	"updated_at" timestamp,
	"deleted_at" timestamp,
	CONSTRAINT "team_invitations_id_unique" UNIQUE("id"),
	CONSTRAINT "team_invitations_invitation_token_unique" UNIQUE("invitation_token")
);
--> statement-breakpoint
CREATE TABLE "team_members" (
	"team_id" uuid NOT NULL,
	"user_id" uuid NOT NULL,
	"role" "team_member_role" DEFAULT 'member' NOT NULL,
	"status" "team_member_status" DEFAULT 'active' NOT NULL,
	"joined_at" timestamp DEFAULT now() NOT NULL,
	"created_at" timestamp DEFAULT now() NOT NULL,
	"updated_at" timestamp,
	"deleted_at" timestamp
);
--> statement-breakpoint
CREATE TABLE "team_projects" (
	"id" uuid PRIMARY KEY NOT NULL,
	"team_id" uuid NOT NULL,
	"project_id" uuid NOT NULL,
	"created_at" timestamp DEFAULT now() NOT NULL,
	"updated_at" timestamp,
	"deleted_at" timestamp,
	CONSTRAINT "team_projects_id_unique" UNIQUE("id")
);
--> statement-breakpoint
CREATE TABLE "teams" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"name" varchar(80) NOT NULL,
	"personal" boolean DEFAULT true NOT NULL,
	"created_by" uuid NOT NULL,
	"created_at" timestamp DEFAULT now() NOT NULL,
	"updated_at" timestamp,
	"deleted_at" timestamp
);
--> statement-breakpoint
CREATE TABLE "users" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"first_name" varchar(50) NOT NULL,
	"last_name" varchar(50) DEFAULT '' NOT NULL,
	"email" varchar(100) NOT NULL,
	"firebase_id" varchar(100) NOT NULL,
	"level" integer DEFAULT 1 NOT NULL,
	"current_team_id" uuid,
	"external_social_id" varchar(100),
	"external_social_type" varchar(25),
	"active" boolean DEFAULT true NOT NULL,
	"email_verified" boolean DEFAULT false NOT NULL,
	"email_verification_code" varchar(100),
	"code_expiration_time" timestamp with time zone DEFAULT now() + interval '30 minutes',
	"created_at" timestamp DEFAULT now() NOT NULL,
	"updated_at" timestamp,
	"deleted_at" timestamp,
	CONSTRAINT "users_email_unique" UNIQUE("email"),
	CONSTRAINT "users_firebase_id_unique" UNIQUE("firebase_id")
);
--> statement-breakpoint
ALTER TABLE "crawler_configs" ADD CONSTRAINT "crawler_configs_index_id_indexes_id_fk" FOREIGN KEY ("index_id") REFERENCES "public"."indexes"("id") ON DELETE cascade ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "indexes" ADD CONSTRAINT "indexes_project_id_projects_id_fk" FOREIGN KEY ("project_id") REFERENCES "public"."projects"("id") ON DELETE cascade ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "indexes" ADD CONSTRAINT "indexes_created_by_users_id_fk" FOREIGN KEY ("created_by") REFERENCES "public"."users"("id") ON DELETE no action ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "private_api_key" ADD CONSTRAINT "private_api_key_user_id_users_id_fk" FOREIGN KEY ("user_id") REFERENCES "public"."users"("id") ON DELETE cascade ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "projects" ADD CONSTRAINT "projects_oramacore_cluster_id_oramacore_clusters_id_fk" FOREIGN KEY ("oramacore_cluster_id") REFERENCES "public"."oramacore_clusters"("id") ON DELETE cascade ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "projects" ADD CONSTRAINT "projects_created_by_users_id_fk" FOREIGN KEY ("created_by") REFERENCES "public"."users"("id") ON DELETE no action ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "team_invitations" ADD CONSTRAINT "team_invitations_team_id_teams_id_fk" FOREIGN KEY ("team_id") REFERENCES "public"."teams"("id") ON DELETE cascade ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "team_invitations" ADD CONSTRAINT "team_invitations_invited_by_users_id_fk" FOREIGN KEY ("invited_by") REFERENCES "public"."users"("id") ON DELETE cascade ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "team_invitations" ADD CONSTRAINT "team_invitations_accepted_by_users_id_fk" FOREIGN KEY ("accepted_by") REFERENCES "public"."users"("id") ON DELETE set null ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "team_members" ADD CONSTRAINT "team_members_team_id_teams_id_fk" FOREIGN KEY ("team_id") REFERENCES "public"."teams"("id") ON DELETE cascade ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "team_members" ADD CONSTRAINT "team_members_user_id_users_id_fk" FOREIGN KEY ("user_id") REFERENCES "public"."users"("id") ON DELETE cascade ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "team_projects" ADD CONSTRAINT "team_projects_team_id_teams_id_fk" FOREIGN KEY ("team_id") REFERENCES "public"."teams"("id") ON DELETE cascade ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "team_projects" ADD CONSTRAINT "team_projects_project_id_projects_id_fk" FOREIGN KEY ("project_id") REFERENCES "public"."projects"("id") ON DELETE cascade ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "teams" ADD CONSTRAINT "teams_created_by_users_id_fk" FOREIGN KEY ("created_by") REFERENCES "public"."users"("id") ON DELETE no action ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "users" ADD CONSTRAINT "users_current_team_id_teams_id_fk" FOREIGN KEY ("current_team_id") REFERENCES "public"."teams"("id") ON DELETE set null ON UPDATE no action;

-- ============ 0001_deep_dragon_lord.sql ============
CREATE TABLE "uploaded_files" (
	"id" uuid PRIMARY KEY NOT NULL,
	"original_filename" varchar(255) NOT NULL,
	"file_type" varchar(50) NOT NULL,
	"file_size_bytes" integer NOT NULL,
	"s3_bucket_key" varchar(500) NOT NULL,
	"status" varchar(20) DEFAULT 'uploading' NOT NULL,
	"uploaded_by" uuid NOT NULL,
	"project_id" uuid NOT NULL,
	"index_id" uuid NOT NULL,
	"error_message" varchar(1000),
	"created_at" timestamp DEFAULT now() NOT NULL,
	"updated_at" timestamp,
	"deleted_at" timestamp,
	CONSTRAINT "uploaded_files_id_unique" UNIQUE("id")
);
--> statement-breakpoint
ALTER TABLE "uploaded_files" ADD CONSTRAINT "uploaded_files_uploaded_by_users_id_fk" FOREIGN KEY ("uploaded_by") REFERENCES "public"."users"("id") ON DELETE no action ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "uploaded_files" ADD CONSTRAINT "uploaded_files_project_id_projects_id_fk" FOREIGN KEY ("project_id") REFERENCES "public"."projects"("id") ON DELETE cascade ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "uploaded_files" ADD CONSTRAINT "uploaded_files_index_id_indexes_id_fk" FOREIGN KEY ("index_id") REFERENCES "public"."indexes"("id") ON DELETE cascade ON UPDATE no action;

-- ============ 0002_open_firebird.sql ============
ALTER TABLE "crawler_configs" ADD COLUMN "client_side_rendering_temp" varchar(4);--> statement-breakpoint
UPDATE "crawler_configs" SET "client_side_rendering_temp" =
  CASE
    WHEN "client_side_rendering" = true THEN 'on'
    WHEN "client_side_rendering" = false THEN 'off'
    ELSE 'auto'
  END;--> statement-breakpoint
ALTER TABLE "crawler_configs" DROP COLUMN "client_side_rendering";--> statement-breakpoint
ALTER TABLE "crawler_configs" RENAME COLUMN "client_side_rendering_temp" TO "client_side_rendering";--> statement-breakpoint
ALTER TABLE "crawler_configs" ALTER COLUMN "client_side_rendering" SET DEFAULT 'auto';--> statement-breakpoint
ALTER TABLE "crawler_configs" ALTER COLUMN "client_side_rendering" SET NOT NULL;


-- ============ 0003_far_black_cat.sql ============
ALTER TABLE "crawler_configs" ALTER COLUMN "client_side_rendering" SET DATA TYPE varchar(8);--> statement-breakpoint
ALTER TABLE "crawler_configs" ALTER COLUMN "client_side_rendering" SET DEFAULT 'auto';

-- ============ 0004_famous_black_panther.sql ============
CREATE TABLE "deleted_indexes" (
	"id" uuid PRIMARY KEY NOT NULL,
	"project_id" uuid NOT NULL,
	"name" varchar(120) NOT NULL,
	"is_airbyte" boolean DEFAULT false NOT NULL,
	"data_source_id" varchar(120) NOT NULL,
	"airbyte_connection_id" varchar(120),
	"is_default" boolean DEFAULT false NOT NULL,
	"processed_at" timestamp,
	"oramacore_index_deleted_at" timestamp,
	"airbyte_connection_deleted_at" timestamp,
	"status" varchar(50) DEFAULT 'pending' NOT NULL,
	"created_at" timestamp DEFAULT now() NOT NULL,
	"updated_at" timestamp,
	"deleted_at" timestamp
);

-- ============ 0005_sleepy_jigsaw.sql ============
-- Custom SQL migration file, put your code below! --

CREATE OR REPLACE FUNCTION capture_deleted_index()
RETURNS TRIGGER AS $$
BEGIN
    INSERT INTO deleted_indexes (
        id, 
        project_id, 
        name, 
        is_airbyte, 
        data_source_id, 
        airbyte_connection_id, 
        is_default
    )
    VALUES (
        OLD.id, 
        OLD.project_id, 
        OLD.name, 
        OLD.is_airbyte, 
        OLD.data_source_id, 
        OLD.airbyte_connection_id, 
        OLD.is_default
        );
    
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE TRIGGER index_delete_trigger
    AFTER DELETE ON indexes
    FOR EACH ROW
    EXECUTE FUNCTION capture_deleted_index();

-- ============ 0006_fixed_wasp.sql ============
ALTER TABLE "deleted_indexes" ADD COLUMN IF NOT EXISTS "oramacore_cluster_id" uuid;


-- ============ 0007_thankful_the_call.sql ============
ALTER TABLE "deleted_indexes" ADD COLUMN IF NOT EXISTS "write_api_key" varchar(255);

-- Create a function to capture project data before deletion
CREATE OR REPLACE FUNCTION public.capture_project_data_before_delete()
 RETURNS trigger
 LANGUAGE plpgsql
AS $function$
BEGIN
    -- Store project data in a temporary table or use a different approach
    -- This ensures we have the data even if the deletion fails later
    
    -- Option 1: Store in a temporary table (session-scoped)
    CREATE TEMP TABLE IF NOT EXISTS temp_deleted_project_data (
        project_id uuid,
        oramacore_cluster_id uuid,
        write_api_key varchar(255)
    );
    
    INSERT INTO temp_deleted_project_data (project_id, oramacore_cluster_id, write_api_key)
    VALUES (OLD.id, OLD.oramacore_cluster_id, OLD.write_api_key);
    
    RETURN OLD;
END;
$function$;

-- Create the BEFORE DELETE trigger on projects
CREATE OR REPLACE TRIGGER capture_project_data_trigger
    BEFORE DELETE ON projects
    FOR EACH ROW
    EXECUTE FUNCTION capture_project_data_before_delete();



CREATE OR REPLACE FUNCTION public.capture_deleted_index()
 RETURNS trigger
 LANGUAGE plpgsql
AS $function$
DECLARE
    cluster_id_var uuid;
    write_api_key_var varchar(255);
BEGIN
    -- Always try the projects table first for index deletions
    SELECT oramacore_cluster_id, write_api_key INTO cluster_id_var, write_api_key_var
    FROM projects 
    WHERE id = OLD.project_id;
    
    -- Only try temp table if projects table lookup failed AND temp table exists
    IF NOT FOUND THEN
        -- Check if temp table exists before trying to use it
        BEGIN
            SELECT oramacore_cluster_id, write_api_key INTO cluster_id_var, write_api_key_var
            FROM temp_deleted_project_data
            WHERE project_id = OLD.project_id;
        EXCEPTION
            WHEN undefined_table THEN
                -- temp_deleted_project_data doesn't exist, which is fine for direct index deletion
                NULL;
        END;
    END IF;
    
    INSERT INTO deleted_indexes (
        id, 
        project_id, 
        name, 
        is_airbyte, 
        data_source_id, 
        airbyte_connection_id, 
        is_default,
        oramacore_cluster_id,
        write_api_key
    )
    VALUES (
        OLD.id, 
        OLD.project_id, 
        OLD.name, 
        OLD.is_airbyte, 
        OLD.data_source_id, 
        OLD.airbyte_connection_id, 
        OLD.is_default,
        cluster_id_var,
        write_api_key_var
    );
    
    RETURN OLD;
END;
$function$;

CREATE OR REPLACE TRIGGER index_delete_trigger
    AFTER DELETE ON indexes
    FOR EACH ROW
    EXECUTE FUNCTION capture_deleted_index();

