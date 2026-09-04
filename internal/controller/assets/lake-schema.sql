-- lake control-plane schema.
--
-- SOURCE OF TRUTH: oramasearch/lake, db/schema.sql. The operator vendors a byte-identical copy at
-- internal/controller/assets/lake-schema.sql and applies it to each Lake's Postgres through a
-- PostgresSchema (one transaction, guarded on the `invitations` table, versioned by content hash).
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
-- Organizations and policy (migration 0004): teams and attested membership, invitations, and
-- one object graph whose attestation-backed grants are evaluated by the policy engine. The
-- missing 0003 is reserved for the identity-core migration developed in parallel.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS objects (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    object_type text        NOT NULL,
    external_id text        NOT NULL,
    parent_id   uuid        REFERENCES objects(id) ON DELETE CASCADE,
    inherit     boolean     NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT objects_type_check CHECK (object_type IN (
        'customer', 'organization', 'team', 'member', 'repository', 'path', 'environment',
        'secret_mount', 'secret')),
    CONSTRAINT objects_not_own_parent CHECK (parent_id IS NULL OR parent_id <> id),
    CONSTRAINT objects_type_external_unique UNIQUE (object_type, external_id)
);
CREATE INDEX IF NOT EXISTS objects_parent_id_idx ON objects (parent_id);
DROP TRIGGER IF EXISTS objects_set_updated_at ON objects;
CREATE TRIGGER objects_set_updated_at BEFORE UPDATE ON objects FOR EACH ROW EXECUTE FUNCTION set_updated_at();

INSERT INTO objects (object_type, external_id)
SELECT 'customer', id::text FROM customers
ON CONFLICT (object_type, external_id) DO NOTHING;

INSERT INTO objects (object_type, external_id, parent_id)
SELECT 'organization', organizations.id::text, customer_objects.id
FROM organizations
LEFT JOIN customers ON customers.organization_id = organizations.id
LEFT JOIN objects AS customer_objects
    ON customer_objects.object_type = 'customer'
    AND customer_objects.external_id = customers.id::text
ON CONFLICT (object_type, external_id) DO NOTHING;

INSERT INTO objects (object_type, external_id, parent_id)
SELECT 'repository', repositories.id::text, customer_objects.id
FROM repositories
JOIN objects AS customer_objects
    ON customer_objects.object_type = 'customer'
    AND customer_objects.external_id = repositories.customer_id::text
ON CONFLICT (object_type, external_id) DO NOTHING;

CREATE TABLE IF NOT EXISTS teams (
    id              uuid         PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid         NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name            varchar(256) NOT NULL,
    display_name    text         NOT NULL DEFAULT '',
    created_at      timestamptz  NOT NULL DEFAULT now(),
    updated_at      timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT teams_name_format CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,254}[a-z0-9])?$'),
    CONSTRAINT teams_organization_name_unique UNIQUE (organization_id, name)
);
CREATE INDEX IF NOT EXISTS teams_organization_id_idx ON teams (organization_id);
DROP TRIGGER IF EXISTS teams_set_updated_at ON teams;
CREATE TRIGGER teams_set_updated_at BEFORE UPDATE ON teams FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS memberships (
    id                    uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id       uuid        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    team_id               uuid        REFERENCES teams(id) ON DELETE CASCADE,
    principal_type        text        NOT NULL,
    principal_id          text        NOT NULL,
    principal_taap_id     text        NOT NULL,
    role                  text        NOT NULL,
    attestation_digest    text        NOT NULL,
    granted_by_taap_id    text        NOT NULL,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT memberships_principal_type_check CHECK (principal_type IN ('user', 'workload')),
    CONSTRAINT memberships_role_check CHECK (role IN ('owner', 'admin', 'member')),
    CONSTRAINT memberships_attestation_check CHECK (
        length(attestation_digest) > 0 AND length(granted_by_taap_id) > 0)
);
CREATE UNIQUE INDEX IF NOT EXISTS memberships_scope_principal_unique
ON memberships (
    organization_id,
    coalesce(team_id, '00000000-0000-0000-0000-000000000000'::uuid),
    principal_type,
    principal_id
);
CREATE INDEX IF NOT EXISTS memberships_organization_id_idx ON memberships (organization_id);
CREATE INDEX IF NOT EXISTS memberships_team_id_idx ON memberships (team_id) WHERE team_id IS NOT NULL;
DROP TRIGGER IF EXISTS memberships_set_updated_at ON memberships;
CREATE TRIGGER memberships_set_updated_at BEFORE UPDATE ON memberships FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS grants (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    principal_taap_id   text        NOT NULL,
    object_id           uuid        NOT NULL REFERENCES objects(id) ON DELETE CASCADE,
    role                text        NOT NULL,
    attestation_digest  text        NOT NULL,
    granted_by_taap_id  text        NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT grants_role_check CHECK (role IN ('read', 'write', 'admin', 'deploy', 'review', 'share')),
    CONSTRAINT grants_attestation_check CHECK (
        length(attestation_digest) > 0 AND length(granted_by_taap_id) > 0),
    CONSTRAINT grants_principal_object_role_unique UNIQUE (principal_taap_id, object_id, role)
);
CREATE INDEX IF NOT EXISTS grants_object_id_idx ON grants (object_id);
CREATE INDEX IF NOT EXISTS grants_principal_taap_id_idx ON grants (principal_taap_id);
DROP TRIGGER IF EXISTS grants_set_updated_at ON grants;
CREATE TRIGGER grants_set_updated_at BEFORE UPDATE ON grants FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS audit_events (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    occurred_at       timestamptz NOT NULL DEFAULT now(),
    acting_principal  text        NOT NULL,
    object_id         uuid        REFERENCES objects(id) ON DELETE SET NULL,
    action            text        NOT NULL,
    outcome           text        NOT NULL,
    request_id        text        NOT NULL,
    detail            jsonb       NOT NULL DEFAULT '{}'::jsonb,
    signature         text,
    previous_digest   text,
    event_digest      text,
    CONSTRAINT audit_events_outcome_check CHECK (outcome IN ('allowed', 'denied', 'error'))
);
CREATE INDEX IF NOT EXISTS audit_events_occurred_at_idx ON audit_events (occurred_at);
CREATE INDEX IF NOT EXISTS audit_events_object_id_idx ON audit_events (object_id) WHERE object_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS invitations (
    id                     uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id        uuid        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    team_id                uuid        REFERENCES teams(id) ON DELETE CASCADE,
    invitee_type           text        NOT NULL,
    invitee                text        NOT NULL,
    role                   text        NOT NULL,
    expires_at             timestamptz NOT NULL,
    status                 text        NOT NULL DEFAULT 'pending',
    accepted_membership_id uuid        REFERENCES memberships(id) ON DELETE SET NULL,
    created_by_taap_id     text        NOT NULL,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT invitations_invitee_type_check CHECK (invitee_type IN ('email', 'handle')),
    CONSTRAINT invitations_role_check CHECK (role IN ('owner', 'admin', 'member')),
    CONSTRAINT invitations_status_check CHECK (status IN ('pending', 'accepted', 'expired', 'revoked')),
    CONSTRAINT invitations_expiry_check CHECK (expires_at > created_at)
);
CREATE INDEX IF NOT EXISTS invitations_organization_id_idx ON invitations (organization_id);
CREATE INDEX IF NOT EXISTS invitations_team_id_idx ON invitations (team_id) WHERE team_id IS NOT NULL;
DROP TRIGGER IF EXISTS invitations_set_updated_at ON invitations;
CREATE TRIGGER invitations_set_updated_at BEFORE UPDATE ON invitations FOR EACH ROW EXECUTE FUNCTION set_updated_at();

INSERT INTO schema_migrations (version) VALUES ('0004_organizations_policy') ON CONFLICT DO NOTHING;
