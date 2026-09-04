-- lake control-plane schema.
--
-- SOURCE OF TRUTH: oramasearch/lake, db/schema.sql. The operator vendors a byte-identical copy at
-- internal/controller/assets/lake-schema.sql and applies it to each Lake's Postgres through a
-- PostgresSchema (one transaction, guarded on the `customers` table, versioned by content hash).
-- Keep the two files identical; the operator's version string changes exactly when this does.
--
-- Postgres 18. Applied with `psql --single-transaction`: no BEGIN/COMMIT here. Idempotent.

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    text        PRIMARY KEY,
    applied_at timestamptz NOT NULL DEFAULT now()
);

CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END $$;

-- ---------------------------------------------------------------------------------------------
-- Identity: who. A user is a person, an organization is a group. Neither owns anything; the
-- tenant that owns everything is the customer below. Identity is TAAP: taap_id is the
-- key-derived identifier (taap:1:<alg>:<base32>) once the actor has a key; nullable until then.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS users (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    taap_id      text        UNIQUE,
    email        text,
    display_name text        NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS users_email_lower_key ON users (lower(email)) WHERE email IS NOT NULL;
DROP TRIGGER IF EXISTS users_set_updated_at ON users;
CREATE TRIGGER users_set_updated_at BEFORE UPDATE ON users FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS organizations (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    taap_id      text        UNIQUE,
    display_name text        NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
DROP TRIGGER IF EXISTS organizations_set_updated_at ON organizations;
CREATE TRIGGER organizations_set_updated_at BEFORE UPDATE ON organizations FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------------------------
-- Tenant: the customer owns everything (repositories, artifacts, secrets, keys, usage carry
-- customer_id ON DELETE CASCADE). A customer is an individual or an organization, never both:
-- customer_type_check ties the type to exactly one non-null identity column, and the UNIQUE
-- identity columns give exactly one customer per user and per organization (NULLs don't collide).
--
-- name is the handle: one global namespace shared by individuals and organizations
-- (customer_name_unique). Lowercase letters, digits and inner hyphens only, 1-256 chars
-- (customer_name_format), so uniqueness is effectively case-insensitive and a handle can never
-- carry spaces or uppercase. No default: the application generates handles
-- ({adjective}-{noun}-{number}) and must pre-check uniqueness to answer 409, not 500.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS customers (
    id              uuid         PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_type   text         NOT NULL,
    user_id         uuid         UNIQUE REFERENCES users(id)         ON DELETE CASCADE,
    organization_id uuid         UNIQUE REFERENCES organizations(id) ON DELETE CASCADE,
    name            varchar(256) NOT NULL,
    created_at      timestamptz  NOT NULL DEFAULT now(),
    updated_at      timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT customer_type_check CHECK (
        (customer_type = 'individual'   AND user_id IS NOT NULL AND organization_id IS NULL) OR
        (customer_type = 'organization' AND organization_id IS NOT NULL AND user_id IS NULL)),
    CONSTRAINT customer_name_unique UNIQUE (name),
    CONSTRAINT customer_name_format CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,254}[a-z0-9])?$')
);
DROP TRIGGER IF EXISTS customers_set_updated_at ON customers;
CREATE TRIGGER customers_set_updated_at BEFORE UPDATE ON customers FOR EACH ROW EXECUTE FUNCTION set_updated_at();

INSERT INTO schema_migrations (version) VALUES ('0001_identity_and_customers') ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- Repositories (migration 0002): owned by a customer, the tenant, and named within it, so the
-- address is <customer handle>/<name>. name is a slug in the handle format; UNIQUE (customer_id,
-- name). visibility is private or public. Deleting the customer deletes its repositories.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS repositories (
    id          uuid         PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id uuid         NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    name        varchar(256) NOT NULL,
    description text         NOT NULL DEFAULT '',
    visibility  text         NOT NULL DEFAULT 'private',
    created_at  timestamptz  NOT NULL DEFAULT now(),
    updated_at  timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT repository_visibility_check CHECK (visibility IN ('private', 'public')),
    CONSTRAINT repository_name_format CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,254}[a-z0-9])?$'),
    CONSTRAINT repository_name_unique UNIQUE (customer_id, name)
);
CREATE INDEX IF NOT EXISTS repositories_customer_id_idx ON repositories (customer_id);
DROP TRIGGER IF EXISTS repositories_set_updated_at ON repositories;
CREATE TRIGGER repositories_set_updated_at BEFORE UPDATE ON repositories FOR EACH ROW EXECUTE FUNCTION set_updated_at();

INSERT INTO schema_migrations (version) VALUES ('0002_repositories') ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- Repository lifecycle (migration 0006): names and owners can change, deletion is a tombstone
-- retained before collection, and organization visibility sits between private and public.
-- repository_lifecycle_events is the migration guard for the operator. audit_events has the
-- shared shape Phase 6 uses until the audit capability adds signing and digest chaining.
-- ---------------------------------------------------------------------------------------------
ALTER TABLE repositories ADD COLUMN IF NOT EXISTS deleted_at timestamptz;
ALTER TABLE repositories ADD COLUMN IF NOT EXISTS retention_until timestamptz;
ALTER TABLE repositories DROP CONSTRAINT IF EXISTS repository_visibility_check;
ALTER TABLE repositories ADD CONSTRAINT repository_visibility_check
    CHECK (visibility IN ('private', 'organization', 'public'));

CREATE INDEX IF NOT EXISTS repositories_live_customer_id_idx
    ON repositories (customer_id) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS repository_refs (
    repository_id uuid        NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    name          varchar(256) NOT NULL,
    head          text        NOT NULL,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (repository_id, name)
);

CREATE TABLE IF NOT EXISTS repository_lifecycle_events (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    repository_id uuid        NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    actor         text        NOT NULL,
    action        text        NOT NULL,
    detail        text        NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS repository_lifecycle_events_repository_id_idx
    ON repository_lifecycle_events (repository_id, created_at DESC);

CREATE TABLE IF NOT EXISTS audit_events (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    actor         text        NOT NULL,
    object_type   text        NOT NULL,
    object_id     uuid        NOT NULL,
    action        text        NOT NULL,
    outcome       text        NOT NULL,
    detail        text        NOT NULL DEFAULT '',
    request_id    text        NOT NULL DEFAULT '',
    signature     bytea,
    previous_hash bytea,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS audit_events_object_id_created_at_idx
    ON audit_events (object_id, created_at DESC);

INSERT INTO schema_migrations (version) VALUES ('0006_repository_lifecycle') ON CONFLICT DO NOTHING;
