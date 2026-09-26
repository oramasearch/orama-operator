-- lake control-plane schema.
--
-- SOURCE OF TRUTH: oramasearch/lake, db/schema.sql. The operator vendors a byte-identical copy at
-- internal/controller/assets/lake-schema.sql and applies it to each Lake's Postgres through a
-- PostgresSchema (one transaction, guarded on the `custodial_identity_guard` table, versioned by
-- content hash).
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
-- one object graph whose attestation-backed grants are evaluated by the policy engine. Migration
-- numbering reflects parallel delivery; identity core follows repository lifecycle as 0007.
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

-- ---------------------------------------------------------------------------------------------
-- Audit (migration 0005): a signed, hash-linked instance stream. sequence is ordering only; the
-- signed predecessor digest makes a gap or reordered row detectable. The append-only trigger
-- protects against application-level rewrites. lake_identity stores all instance identities so
-- events signed before a rotation remain verifiable. A generated identity's wrapped_seed is
-- AES-256-GCM ciphertext under the customer-independent instance KEK; private seed material is
-- never returned by lake. Every capability call, authentication and session event is one row;
-- the store methods of migration 0004 no longer write their own provisional rows.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS lake_identity (
    id                uuid        PRIMARY KEY,
    taap_id           text        NOT NULL UNIQUE,
    public_key        text        NOT NULL,
    wrapped_seed      text        NOT NULL,
    created_at        timestamptz NOT NULL,
    rotation_document jsonb,
    current           boolean     NOT NULL DEFAULT true
);
CREATE UNIQUE INDEX IF NOT EXISTS lake_identity_one_current_idx ON lake_identity (current) WHERE current;

CREATE TABLE IF NOT EXISTS audit_events (
    sequence        bigserial    PRIMARY KEY,
    id              uuid         NOT NULL UNIQUE,
    occurred_at     timestamptz  NOT NULL,
    customer_id     uuid         REFERENCES customers(id) ON DELETE RESTRICT,
    kind            text         NOT NULL,
    principal       text         NOT NULL,
    object          text         NOT NULL,
    action          text         NOT NULL,
    outcome         text         NOT NULL,
    request_id      text         NOT NULL,
    method          text,
    device          text,
    details         jsonb        NOT NULL DEFAULT '{}'::jsonb,
    previous_digest text,
    signer          text         NOT NULL REFERENCES lake_identity(taap_id) ON DELETE RESTRICT,
    signature       text         NOT NULL,
    digest          text         NOT NULL UNIQUE,
    CONSTRAINT audit_events_outcome_check CHECK (outcome IN ('success', 'failure', 'denied', 'revoked')),
    CONSTRAINT audit_events_digest_format CHECK (digest ~ '^[0-9a-f]{64}$'),
    CONSTRAINT audit_events_predecessor_format CHECK (previous_digest IS NULL OR previous_digest ~ '^[0-9a-f]{64}$')
);
CREATE INDEX IF NOT EXISTS audit_events_principal_occurred_at_idx ON audit_events (principal, occurred_at);
CREATE INDEX IF NOT EXISTS audit_events_object_occurred_at_idx ON audit_events (object, occurred_at);
CREATE INDEX IF NOT EXISTS audit_events_outcome_occurred_at_idx ON audit_events (outcome, occurred_at);
CREATE INDEX IF NOT EXISTS audit_events_customer_occurred_at_idx ON audit_events (customer_id, occurred_at);

CREATE OR REPLACE FUNCTION reject_audit_event_rewrite() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only';
END $$;
DROP TRIGGER IF EXISTS audit_events_reject_update ON audit_events;
CREATE TRIGGER audit_events_reject_update BEFORE UPDATE ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_audit_event_rewrite();
DROP TRIGGER IF EXISTS audit_events_reject_delete ON audit_events;
CREATE TRIGGER audit_events_reject_delete BEFORE DELETE ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_audit_event_rewrite();

CREATE TABLE IF NOT EXISTS audit_retention (
    customer_id    uuid        PRIMARY KEY REFERENCES customers(id) ON DELETE CASCADE,
    retention_days integer     NOT NULL,
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT audit_retention_floor_check CHECK (retention_days >= 400)
);
DROP TRIGGER IF EXISTS audit_retention_set_updated_at ON audit_retention;
CREATE TRIGGER audit_retention_set_updated_at BEFORE UPDATE ON audit_retention FOR EACH ROW EXECUTE FUNCTION set_updated_at();

INSERT INTO schema_migrations (version) VALUES ('0005_audit') ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- Repository lifecycle (migration 0006): names and owners can change, deletion is a tombstone
-- retained before collection, and organization visibility sits between private and public.
-- repository_refs is the migration guard for the operator. Lifecycle operations are recorded by
-- the signed audit stream above through the capability registry.
-- ---------------------------------------------------------------------------------------------
ALTER TABLE repositories ADD COLUMN IF NOT EXISTS deleted_at timestamptz;
ALTER TABLE repositories ADD COLUMN IF NOT EXISTS retention_until timestamptz;
ALTER TABLE repositories DROP CONSTRAINT IF EXISTS repository_visibility_check;
ALTER TABLE repositories ADD CONSTRAINT repository_visibility_check
    CHECK (visibility IN ('private', 'organization', 'public'));

CREATE INDEX IF NOT EXISTS repositories_live_customer_id_idx
    ON repositories (customer_id) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS repository_refs (
    repository_id uuid         NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    name          varchar(256) NOT NULL,
    head          text         NOT NULL,
    updated_at    timestamptz  NOT NULL DEFAULT now(),
    PRIMARY KEY (repository_id, name)
);

INSERT INTO schema_migrations (version) VALUES ('0006_repository_lifecycle') ON CONFLICT DO NOTHING;
-- Identity core (migration 0007): credentials prove control of a user's durable TAAP identity;
-- sessions are row-resolved bearer credentials, so revocation is effective on the next request.
-- Each migration ends in a guard table. The operator uses the newest one until its incremental
-- migration work (orama-operator #126) lands.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS identity_documents (
    user_id       uuid        PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    document      jsonb       NOT NULL,
    document_hash text        NOT NULL UNIQUE,
    verified_at   timestamptz NOT NULL DEFAULT now(),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
DROP TRIGGER IF EXISTS identity_documents_set_updated_at ON identity_documents;
CREATE TRIGGER identity_documents_set_updated_at BEFORE UPDATE ON identity_documents FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS devices (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         text        NOT NULL,
    revoked_at   timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS devices_user_id_idx ON devices (user_id, created_at DESC);
DROP TRIGGER IF EXISTS devices_set_updated_at ON devices;
CREATE TRIGGER devices_set_updated_at BEFORE UPDATE ON devices FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS credentials (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    pool_id      text        NOT NULL DEFAULT 'lake',
    kind         text        NOT NULL,
    subject      text        NOT NULL,
    secret_hash  text,
    public_data  jsonb       NOT NULL DEFAULT '{}'::jsonb,
    revoked_at   timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT credentials_kind_check CHECK (kind IN ('password', 'totp', 'passkey', 'oauth', 'admin')),
    CONSTRAINT credentials_subject_unique UNIQUE (pool_id, kind, subject)
);
CREATE INDEX IF NOT EXISTS credentials_user_id_idx ON credentials (user_id, created_at DESC);
DROP TRIGGER IF EXISTS credentials_set_updated_at ON credentials;
CREATE TRIGGER credentials_set_updated_at BEFORE UPDATE ON credentials FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS sessions (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid        REFERENCES users(id) ON DELETE CASCADE,
    device_id    uuid        REFERENCES devices(id) ON DELETE SET NULL,
    pool_id      text        NOT NULL DEFAULT 'lake',
    kind         text        NOT NULL DEFAULT 'browser',
    name         text        NOT NULL DEFAULT '',
    secret_hash  text        NOT NULL UNIQUE,
    scopes       text[]      NOT NULL DEFAULT ARRAY[]::text[],
    reason       text        NOT NULL DEFAULT '',
    expires_at   timestamptz,
    revoked_at   timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    CONSTRAINT sessions_kind_check
        CHECK (kind IN ('browser', 'api_token', 'static', 'admin', 'taap')),
    CONSTRAINT static_session_reason_check CHECK (kind <> 'static' OR length(reason) > 0)
);
CREATE INDEX IF NOT EXISTS sessions_user_id_idx ON sessions (user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS sessions_device_id_idx ON sessions (device_id) WHERE device_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS keystore (
    purpose     text        PRIMARY KEY,
    wrapped_key bytea       NOT NULL,
    version     integer     NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
DROP TRIGGER IF EXISTS keystore_set_updated_at ON keystore;
CREATE TRIGGER keystore_set_updated_at BEFORE UPDATE ON keystore FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS oauth_providers (
    pool_id               text        NOT NULL,
    provider              text        NOT NULL,
    issuer                text        NOT NULL,
    client_id             text        NOT NULL,
    encrypted_client_secret bytea     NOT NULL,
    scopes                text[]      NOT NULL,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (pool_id, provider)
);
DROP TRIGGER IF EXISTS oauth_providers_set_updated_at ON oauth_providers;
CREATE TRIGGER oauth_providers_set_updated_at BEFORE UPDATE ON oauth_providers FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS identity_oauth_flows (
    id                      uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    pool_id                 text        NOT NULL,
    provider                text        NOT NULL,
    state_hash              text        NOT NULL UNIQUE,
    encrypted_code_verifier bytea       NOT NULL,
    nonce                   text        NOT NULL,
    device_name             text        NOT NULL,
    identity_document       text        NOT NULL DEFAULT '',
    expires_at              timestamptz NOT NULL,
    used_at                 timestamptz,
    created_at              timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (pool_id, provider) REFERENCES oauth_providers(pool_id, provider) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS identity_oauth_flows_lookup_idx ON identity_oauth_flows (state_hash, expires_at) WHERE used_at IS NULL;

CREATE TABLE IF NOT EXISTS identity_webauthn_challenges (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid        REFERENCES users(id) ON DELETE CASCADE,
    pool_id     text        NOT NULL,
    ceremony    text        NOT NULL,
    challenge   text        NOT NULL UNIQUE,
    rp_id       text        NOT NULL,
    origin      text        NOT NULL,
    expires_at  timestamptz NOT NULL,
    used_at     timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT identity_webauthn_challenges_ceremony_check CHECK (ceremony IN ('registration', 'assertion'))
);
ALTER TABLE identity_webauthn_challenges ADD COLUMN IF NOT EXISTS rp_id text NOT NULL DEFAULT '';
ALTER TABLE identity_webauthn_challenges ADD COLUMN IF NOT EXISTS origin text NOT NULL DEFAULT '';
ALTER TABLE identity_webauthn_challenges ALTER COLUMN rp_id DROP DEFAULT;
ALTER TABLE identity_webauthn_challenges ALTER COLUMN origin DROP DEFAULT;
CREATE INDEX IF NOT EXISTS identity_webauthn_challenges_lookup_idx ON identity_webauthn_challenges (challenge, expires_at) WHERE used_at IS NULL;

CREATE TABLE IF NOT EXISTS identity_core_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES ('0007_identity_core') ON CONFLICT DO NOTHING;

-- MFA recovery (migration 0008): recovery codes are password-grade hashes, used once and revoked.
ALTER TABLE credentials DROP CONSTRAINT IF EXISTS credentials_kind_check;
ALTER TABLE credentials ADD CONSTRAINT credentials_kind_check
    CHECK (kind IN ('password', 'totp', 'recovery', 'passkey', 'oauth', 'admin'));

CREATE TABLE IF NOT EXISTS identity_mfa_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES ('0008_mfa_recovery') ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- Secrets and encryption (migration 0009): organization mounts in a customer namespace,
-- versioned static values, and named encryption keys. The existing keystore gains one row per
-- customer KEK; that KEK wraps every mount and transit data key in secrets_data_keys. Rotating a
-- customer KEK updates only wrapped data keys, never stored secret ciphertext.
-- ---------------------------------------------------------------------------------------------
ALTER TABLE keystore ADD COLUMN IF NOT EXISTS customer_id uuid REFERENCES customers(id) ON DELETE CASCADE;
CREATE UNIQUE INDEX IF NOT EXISTS keystore_customer_id_unique
    ON keystore (customer_id) WHERE customer_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS secrets_mounts (
    id              uuid         PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id     uuid         NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    organization_id uuid         NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name            varchar(256) NOT NULL,
    description     text         NOT NULL DEFAULT '',
    created_at      timestamptz  NOT NULL DEFAULT now(),
    updated_at      timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT secrets_mounts_name_format
        CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,254}[a-z0-9])?$'),
    CONSTRAINT secrets_mounts_scope_name_unique UNIQUE (customer_id, organization_id, name)
);
CREATE INDEX IF NOT EXISTS secrets_mounts_organization_id_idx
    ON secrets_mounts (organization_id, created_at DESC);
DROP TRIGGER IF EXISTS secrets_mounts_set_updated_at ON secrets_mounts;
CREATE TRIGGER secrets_mounts_set_updated_at BEFORE UPDATE ON secrets_mounts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS secrets_entries (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    mount_id        uuid        NOT NULL REFERENCES secrets_mounts(id) ON DELETE CASCADE,
    name            text        NOT NULL,
    current_version bigint      NOT NULL DEFAULT 0,
    custom_metadata jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT secrets_entries_name_check CHECK (length(name) BETWEEN 1 AND 1024),
    CONSTRAINT secrets_entries_mount_name_unique UNIQUE (mount_id, name)
);
CREATE INDEX IF NOT EXISTS secrets_entries_mount_id_idx
    ON secrets_entries (mount_id, created_at DESC);
DROP TRIGGER IF EXISTS secrets_entries_set_updated_at ON secrets_entries;
CREATE TRIGGER secrets_entries_set_updated_at BEFORE UPDATE ON secrets_entries
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS secrets_versions (
    entry_id     uuid        NOT NULL REFERENCES secrets_entries(id) ON DELETE CASCADE,
    version      bigint      NOT NULL,
    ciphertext   text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    deleted_at   timestamptz,
    destroyed_at timestamptz,
    PRIMARY KEY (entry_id, version),
    CONSTRAINT secrets_versions_number_check CHECK (version > 0),
    CONSTRAINT secrets_versions_destroyed_check
        CHECK (destroyed_at IS NULL OR (ciphertext IS NULL AND deleted_at IS NOT NULL))
);

CREATE TABLE IF NOT EXISTS secrets_encryption_keys (
    id              uuid         PRIMARY KEY DEFAULT gen_random_uuid(),
    mount_id        uuid         NOT NULL REFERENCES secrets_mounts(id) ON DELETE CASCADE,
    name            varchar(256) NOT NULL,
    derived_from_id uuid         REFERENCES secrets_encryption_keys(id) ON DELETE RESTRICT,
    current_version bigint       NOT NULL DEFAULT 1,
    created_at      timestamptz  NOT NULL DEFAULT now(),
    updated_at      timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT secrets_encryption_keys_name_format
        CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,254}[a-z0-9])?$'),
    CONSTRAINT secrets_encryption_keys_version_check CHECK (current_version > 0),
    CONSTRAINT secrets_encryption_keys_mount_name_unique UNIQUE (mount_id, name)
);
CREATE INDEX IF NOT EXISTS secrets_encryption_keys_mount_id_idx
    ON secrets_encryption_keys (mount_id, created_at DESC);
DROP TRIGGER IF EXISTS secrets_encryption_keys_set_updated_at ON secrets_encryption_keys;
CREATE TRIGGER secrets_encryption_keys_set_updated_at BEFORE UPDATE ON secrets_encryption_keys
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS secrets_data_keys (
    customer_id uuid        NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    owner_kind  text        NOT NULL,
    owner_id    uuid        NOT NULL,
    key_version bigint      NOT NULL,
    wrapped_key text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (owner_kind, owner_id, key_version),
    CONSTRAINT secrets_data_keys_owner_kind_check CHECK (owner_kind IN ('mount', 'encryption')),
    CONSTRAINT secrets_data_keys_version_check CHECK (key_version > 0)
);
CREATE INDEX IF NOT EXISTS secrets_data_keys_customer_id_idx ON secrets_data_keys (customer_id);

CREATE TABLE IF NOT EXISTS secrets_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES ('0009_secrets') ON CONFLICT DO NOTHING;

-- SSO and federation (migration 0010): enterprise identity providers, SCIM resources, and the
-- organization policy that names which issuer may assert each claim. The provider records only
-- encrypted client secrets and SCIM bearer-token hashes. Deactivating a SCIM user revokes every
-- one of that user's sessions in the same transaction.
-- ---------------------------------------------------------------------------------------------
ALTER TABLE credentials DROP CONSTRAINT IF EXISTS credentials_kind_check;
ALTER TABLE credentials ADD CONSTRAINT credentials_kind_check
    CHECK (kind IN ('password', 'totp', 'recovery', 'passkey', 'oauth', 'saml', 'oidc', 'admin'));

CREATE TABLE IF NOT EXISTS sso_providers (
    id                      uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id         uuid        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    pool_id                 text        NOT NULL DEFAULT 'lake',
    name                    varchar(256) NOT NULL,
    protocol                text        NOT NULL,
    issuer                  text        NOT NULL,
    metadata                text        NOT NULL DEFAULT '',
    signing_certificate     text        NOT NULL DEFAULT '',
    client_id               text        NOT NULL DEFAULT '',
    encrypted_client_secret bytea,
    scim_token_hash         text        NOT NULL,
    attribute_mapping       jsonb       NOT NULL DEFAULT '{}'::jsonb,
    jit_provisioning        boolean     NOT NULL DEFAULT false,
    enabled                 boolean     NOT NULL DEFAULT true,
    tested_at               timestamptz,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT sso_providers_protocol_check CHECK (protocol IN ('saml', 'oidc')),
    CONSTRAINT sso_providers_name_unique UNIQUE (organization_id, name),
    CONSTRAINT sso_providers_issuer_unique UNIQUE (organization_id, issuer)
);
CREATE INDEX IF NOT EXISTS sso_providers_organization_id_idx
    ON sso_providers (organization_id, created_at DESC);
DROP TRIGGER IF EXISTS sso_providers_set_updated_at ON sso_providers;
CREATE TRIGGER sso_providers_set_updated_at BEFORE UPDATE ON sso_providers
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS federation_issuers (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    issuer          text        NOT NULL,
    claim           varchar(256) NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT federation_issuers_claim_check CHECK (length(claim) > 0),
    CONSTRAINT federation_issuers_unique UNIQUE (organization_id, issuer, claim)
);
CREATE INDEX IF NOT EXISTS federation_issuers_lookup_idx
    ON federation_issuers (organization_id, issuer, claim);

CREATE TABLE IF NOT EXISTS scim_users (
    provider_id  uuid        NOT NULL REFERENCES sso_providers(id) ON DELETE CASCADE,
    external_id  text        NOT NULL,
    user_id      uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    active       boolean     NOT NULL DEFAULT true,
    attributes   jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider_id, external_id),
    CONSTRAINT scim_users_provider_user_unique UNIQUE (provider_id, user_id)
);
DROP TRIGGER IF EXISTS scim_users_set_updated_at ON scim_users;
CREATE TRIGGER scim_users_set_updated_at BEFORE UPDATE ON scim_users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS scim_groups (
    provider_id  uuid        NOT NULL REFERENCES sso_providers(id) ON DELETE CASCADE,
    external_id  text        NOT NULL,
    team_id      uuid        NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    display_name text        NOT NULL,
    attributes   jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider_id, external_id),
    CONSTRAINT scim_groups_provider_team_unique UNIQUE (provider_id, team_id)
);
DROP TRIGGER IF EXISTS scim_groups_set_updated_at ON scim_groups;
CREATE TRIGGER scim_groups_set_updated_at BEFORE UPDATE ON scim_groups
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS scim_group_members (
    provider_id       uuid        NOT NULL,
    group_external_id text        NOT NULL,
    user_external_id  text        NOT NULL,
    membership_id     uuid        NOT NULL REFERENCES memberships(id) ON DELETE CASCADE,
    created_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider_id, group_external_id, user_external_id),
    FOREIGN KEY (provider_id, group_external_id)
        REFERENCES scim_groups(provider_id, external_id) ON DELETE CASCADE,
    FOREIGN KEY (provider_id, user_external_id)
        REFERENCES scim_users(provider_id, external_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS sso_provisioning_logs (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_id  uuid        NOT NULL REFERENCES sso_providers(id) ON DELETE CASCADE,
    resource     text        NOT NULL,
    operation    text        NOT NULL,
    external_id  text        NOT NULL DEFAULT '',
    outcome      text        NOT NULL,
    details      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT sso_provisioning_logs_resource_check CHECK (resource IN ('user', 'group')),
    CONSTRAINT sso_provisioning_logs_operation_check CHECK (operation IN ('create', 'update', 'deactivate')),
    CONSTRAINT sso_provisioning_logs_outcome_check CHECK (outcome IN ('success', 'failure'))
);
CREATE INDEX IF NOT EXISTS sso_provisioning_logs_provider_id_idx
    ON sso_provisioning_logs (provider_id, created_at DESC);

CREATE TABLE IF NOT EXISTS sso_federation_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES ('0010_sso_federation') ON CONFLICT DO NOTHING;

-- Offline grants and account recovery (migration 0011): signed, bounded cached-read grants;
-- one-use recovery paths; device notification; two-administrator organization recovery; and
-- client-wrapped repository key custody. Prefix every table for parallel wave ownership.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS offline_recovery_grants (
    id                uuid        PRIMARY KEY,
    user_id           uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    principal_taap_id text        NOT NULL,
    device_id         uuid        NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    scopes            text[]      NOT NULL,
    credential        jsonb       NOT NULL,
    issued_at         timestamptz NOT NULL DEFAULT now(),
    expires_at        timestamptz NOT NULL,
    revoked_at        timestamptz,
    CONSTRAINT offline_recovery_grants_scope_check CHECK (cardinality(scopes) > 0),
    CONSTRAINT offline_recovery_grants_expiry_check CHECK (expires_at > issued_at)
);
CREATE INDEX IF NOT EXISTS offline_recovery_grants_user_id_idx
    ON offline_recovery_grants (user_id, issued_at DESC);

CREATE TABLE IF NOT EXISTS offline_recovery_attempts (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    organization_id uuid        REFERENCES organizations(id) ON DELETE CASCADE,
    method          text        NOT NULL,
    status          text        NOT NULL,
    token_hash      text,
    ready_at        timestamptz NOT NULL,
    expires_at      timestamptz NOT NULL,
    completed_at    timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT offline_recovery_attempts_method_check CHECK (method IN ('email', 'admin')),
    CONSTRAINT offline_recovery_attempts_status_check
        CHECK (status IN ('cooling', 'pending_approval', 'approved', 'completed')),
    CONSTRAINT offline_recovery_attempts_expiry_check CHECK (expires_at > ready_at)
);
CREATE INDEX IF NOT EXISTS offline_recovery_attempts_user_id_idx
    ON offline_recovery_attempts (user_id, created_at DESC);

CREATE TABLE IF NOT EXISTS offline_recovery_admin_approvals (
    recovery_id      uuid        NOT NULL REFERENCES offline_recovery_attempts(id) ON DELETE CASCADE,
    approver_taap_id text        NOT NULL,
    approved_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (recovery_id, approver_taap_id)
);

CREATE TABLE IF NOT EXISTS offline_recovery_deliveries (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    recovery_id  uuid        NOT NULL REFERENCES offline_recovery_attempts(id) ON DELETE CASCADE,
    email        text        NOT NULL,
    token        text,
    kind         text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    delivered_at timestamptz,
    CONSTRAINT offline_recovery_deliveries_kind_check
        CHECK (kind IN ('email_recovery_started', 'admin_recovery_approved'))
);
CREATE INDEX IF NOT EXISTS offline_recovery_deliveries_pending_idx
    ON offline_recovery_deliveries (created_at) WHERE delivered_at IS NULL;

CREATE TABLE IF NOT EXISTS offline_recovery_notifications (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    recovery_id uuid        NOT NULL REFERENCES offline_recovery_attempts(id) ON DELETE CASCADE,
    user_id     uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_id   uuid        NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    kind        text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS offline_recovery_notifications_user_id_idx
    ON offline_recovery_notifications (user_id, created_at DESC);

CREATE TABLE IF NOT EXISTS offline_recovery_contacts (
    repository_id   uuid        PRIMARY KEY REFERENCES repositories(id) ON DELETE CASCADE,
    contact_taap_id text        NOT NULL,
    wrapped_key     text        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT offline_recovery_contacts_key_check CHECK (length(wrapped_key) > 0)
);
DROP TRIGGER IF EXISTS offline_recovery_contacts_set_updated_at ON offline_recovery_contacts;
CREATE TRIGGER offline_recovery_contacts_set_updated_at BEFORE UPDATE ON offline_recovery_contacts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS offline_recovery_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES ('0011_offline_recovery') ON CONFLICT DO NOTHING;

-- Forge, export, and GitHub import (migration 0012): a versioned value per
-- durable forge fact. Review anchors remain values so libliquid map_anchor can
-- replace the temporary line mapping without changing persistent columns.
CREATE TABLE IF NOT EXISTS forge_records (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    repository_id uuid        NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    kind          text        NOT NULL,
    value         jsonb       NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT forge_records_kind_check CHECK (kind IN (
        'issue', 'issue_comment', 'review', 'review_comment', 'release',
        'webhook', 'webhook_delivery', 'branch_protection', 'export', 'github_import'
    )),
    CONSTRAINT forge_records_value_check CHECK (jsonb_typeof(value) = 'object')
);
CREATE INDEX IF NOT EXISTS forge_records_repository_kind_created_idx
    ON forge_records (repository_id, kind, created_at DESC);
DROP TRIGGER IF EXISTS forge_records_set_updated_at ON forge_records;
CREATE TRIGGER forge_records_set_updated_at BEFORE UPDATE ON forge_records
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS forge_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES ('0012_forge_export_import') ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- PKI, caller attestation, and break-glass recovery (migration 0013). CA private material is
-- envelope-encrypted before persistence; certificate and recovery records retain only public
-- values and audit references.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS pki_authorities (
    id                      uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id             uuid        NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    root_key_ciphertext     text        NOT NULL,
    intermediate_ciphertext text        NOT NULL,
    root_certificate_pem    text        NOT NULL,
    intermediate_chain_pem  text        NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pki_authorities_customer_unique UNIQUE (customer_id)
);
DROP TRIGGER IF EXISTS pki_authorities_set_updated_at ON pki_authorities;
CREATE TRIGGER pki_authorities_set_updated_at BEFORE UPDATE ON pki_authorities
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS pki_certificates (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    authority_id    uuid        NOT NULL REFERENCES pki_authorities(id) ON DELETE CASCADE,
    customer_id     uuid        NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    profile         text        NOT NULL,
    subject         text        NOT NULL,
    serial          text        NOT NULL,
    public_value    text        NOT NULL,
    expires_at      timestamptz NOT NULL,
    issued_at       timestamptz NOT NULL DEFAULT now(),
    renewed_from_id uuid        REFERENCES pki_certificates(id) ON DELETE SET NULL,
    revoked_at      timestamptz,
    CONSTRAINT pki_certificates_profile_check
        CHECK (profile IN ('x509_svid', 'jwt_svid', 'acme', 'ssh', 'client', 'device', 'agent')),
    CONSTRAINT pki_certificates_authority_serial_unique UNIQUE (authority_id, serial)
);
CREATE INDEX IF NOT EXISTS pki_certificates_customer_id_idx
    ON pki_certificates (customer_id, issued_at DESC);

CREATE TABLE IF NOT EXISTS pki_acme_orders (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id     uuid        NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    certificate_id  uuid        REFERENCES pki_certificates(id) ON DELETE SET NULL,
    identifiers     jsonb       NOT NULL,
    account_key     text        NOT NULL,
    status          text        NOT NULL DEFAULT 'pending',
    expires_at      timestamptz NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pki_acme_orders_status_check
        CHECK (status IN ('pending', 'ready', 'valid', 'invalid'))
);
DROP TRIGGER IF EXISTS pki_acme_orders_set_updated_at ON pki_acme_orders;
CREATE TRIGGER pki_acme_orders_set_updated_at BEFORE UPDATE ON pki_acme_orders
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS pki_callers (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id         uuid        NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    workload_taap_id    text        NOT NULL,
    attestation_type    text        NOT NULL,
    issuer              text        NOT NULL DEFAULT '',
    subject             text        NOT NULL DEFAULT '',
    attributes          jsonb       NOT NULL DEFAULT '{}'::jsonb,
    expires_at          timestamptz NOT NULL,
    issued_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pki_callers_attestation_type_check
        CHECK (attestation_type IN ('kubernetes', 'oidc', 'jwt', 'aws', 'azure'))
);
CREATE INDEX IF NOT EXISTS pki_callers_customer_id_idx
    ON pki_callers (customer_id, issued_at DESC);

CREATE TABLE IF NOT EXISTS pki_recovery_ceremonies (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id     uuid        NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    authority_id    uuid        NOT NULL REFERENCES pki_authorities(id) ON DELETE CASCADE,
    threshold       integer     NOT NULL,
    share_count     integer     NOT NULL,
    recovered_at    timestamptz,
    expires_at      timestamptz NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pki_recovery_ceremonies_threshold_check
        CHECK (threshold > 1 AND share_count >= threshold)
);

CREATE TABLE IF NOT EXISTS pki_recovery_shares (
    ceremony_id     uuid        NOT NULL REFERENCES pki_recovery_ceremonies(id) ON DELETE CASCADE,
    share_index     integer     NOT NULL,
    share_digest    text        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (ceremony_id, share_index),
    CONSTRAINT pki_recovery_shares_index_check CHECK (share_index > 0)
);

CREATE TABLE IF NOT EXISTS pki_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES ('0013_pki') ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- Metering (migration 0014): hourly customer usage and an idempotency ledger for build compute.
-- Stored bytes are a gauge; transfer and compute values are counters.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS metering_usage (
    customer_id        uuid        NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    hour               timestamptz NOT NULL,
    stored_bytes       bigint      NOT NULL DEFAULT 0 CHECK (stored_bytes >= 0),
    transfer_in_bytes  bigint      NOT NULL DEFAULT 0 CHECK (transfer_in_bytes >= 0),
    transfer_out_bytes bigint      NOT NULL DEFAULT 0 CHECK (transfer_out_bytes >= 0),
    compute_seconds    bigint      NOT NULL DEFAULT 0 CHECK (compute_seconds >= 0),
    PRIMARY KEY (customer_id, hour),
    CONSTRAINT metering_usage_hour_check CHECK (hour = date_trunc('hour', hour))
);

CREATE TABLE IF NOT EXISTS metering_build_records (
    customer_id     uuid        NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    result_id       text        NOT NULL,
    hour            timestamptz NOT NULL,
    compute_seconds bigint     NOT NULL CHECK (compute_seconds >= 0),
    PRIMARY KEY (customer_id, result_id),
    CONSTRAINT metering_build_records_hour_check CHECK (hour = date_trunc('hour', hour))
);

CREATE TABLE IF NOT EXISTS metering_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES ('0014_metering') ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- Drive sharing (migration 0015): expiring, scoped link capabilities over path policy objects.
-- Direct recipient shares remain ordinary grants on the same objects.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS drive_share_links (
    id                    uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    repository_id         uuid        NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    object_id             uuid        NOT NULL REFERENCES objects(id) ON DELETE CASCADE,
    node_id               text        NOT NULL,
    path                  text        NOT NULL,
    role                  text        NOT NULL,
    token_hash            text        NOT NULL UNIQUE,
    expires_at            timestamptz NOT NULL,
    revoked_at            timestamptz,
    created_by_taap_id    text        NOT NULL,
    created_at            timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT drive_share_links_role_check CHECK (role IN ('read', 'write')),
    CONSTRAINT drive_share_links_node_check CHECK (length(node_id) = 32),
    CONSTRAINT drive_share_links_expiry_check CHECK (expires_at > created_at)
);
CREATE INDEX IF NOT EXISTS drive_share_links_repository_object_idx
    ON drive_share_links (repository_id, object_id, created_at DESC);

CREATE TABLE IF NOT EXISTS drive_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES ('0015_drive') ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- Application authentication service (migration 0016): isolated user pools, hosted identity
-- configuration, per-application signing keys, lifecycle webhooks, and operator-managed users.
-- Private signing seeds and webhook secrets are encrypted through application-specific keystore
-- purposes before they enter these tables.
-- ---------------------------------------------------------------------------------------------
ALTER TABLE objects DROP CONSTRAINT IF EXISTS objects_type_check;
ALTER TABLE objects ADD CONSTRAINT objects_type_check CHECK (object_type IN (
    'customer', 'organization', 'team', 'member', 'repository', 'path', 'environment',
    'secret_mount', 'secret', 'application'));

ALTER TABLE users ADD COLUMN IF NOT EXISTS pool_id text NOT NULL DEFAULT 'lake';
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_taap_id_key;
DROP INDEX IF EXISTS users_email_lower_key;
CREATE UNIQUE INDEX IF NOT EXISTS auth_app_users_pool_taap_unique
    ON users (pool_id, taap_id) WHERE taap_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS auth_app_users_pool_email_unique
    ON users (pool_id, lower(email)) WHERE email IS NOT NULL;

CREATE TABLE IF NOT EXISTS auth_applications (
    id                    uuid        PRIMARY KEY,
    customer_id           uuid        NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    pool_id               text        NOT NULL UNIQUE,
    name                  text        NOT NULL,
    allowed_origins       jsonb       NOT NULL,
    redirect_uris         jsonb       NOT NULL DEFAULT '[]'::jsonb,
    custom_sign_in_domain text,
    theme                 jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT auth_applications_name_check CHECK (length(name) BETWEEN 1 AND 256),
    CONSTRAINT auth_applications_origins_check CHECK (
        jsonb_typeof(allowed_origins) = 'array' AND jsonb_array_length(allowed_origins) > 0),
    CONSTRAINT auth_applications_redirects_check CHECK (jsonb_typeof(redirect_uris) = 'array'),
    CONSTRAINT auth_applications_theme_check CHECK (jsonb_typeof(theme) = 'object')
);
CREATE UNIQUE INDEX IF NOT EXISTS auth_applications_custom_domain_unique
    ON auth_applications (lower(custom_sign_in_domain))
    WHERE custom_sign_in_domain IS NOT NULL;
CREATE INDEX IF NOT EXISTS auth_applications_customer_idx
    ON auth_applications (customer_id, created_at DESC);
DROP TRIGGER IF EXISTS auth_applications_set_updated_at ON auth_applications;
CREATE TRIGGER auth_applications_set_updated_at BEFORE UPDATE ON auth_applications
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS auth_app_signing_keys (
    application_id uuid        NOT NULL REFERENCES auth_applications(id) ON DELETE CASCADE,
    kid            text        NOT NULL UNIQUE,
    version        integer     NOT NULL CHECK (version > 0),
    encrypted_seed text        NOT NULL,
    public_key     text        NOT NULL,
    current        boolean     NOT NULL DEFAULT true,
    retired_at     timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (application_id, version)
);
CREATE UNIQUE INDEX IF NOT EXISTS auth_app_signing_keys_current_unique
    ON auth_app_signing_keys (application_id) WHERE current;

CREATE TABLE IF NOT EXISTS auth_app_users (
    application_id uuid        NOT NULL REFERENCES auth_applications(id) ON DELETE CASCADE,
    user_id        uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    status         text        NOT NULL DEFAULT 'active',
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (application_id, user_id),
    CONSTRAINT auth_app_users_status_check CHECK (status IN ('active', 'suspended'))
);
CREATE INDEX IF NOT EXISTS auth_app_users_status_idx
    ON auth_app_users (application_id, status, created_at DESC);
DROP TRIGGER IF EXISTS auth_app_users_set_updated_at ON auth_app_users;
CREATE TRIGGER auth_app_users_set_updated_at BEFORE UPDATE ON auth_app_users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS auth_app_organizations (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id uuid        NOT NULL REFERENCES auth_applications(id) ON DELETE CASCADE,
    taap_id        text        NOT NULL,
    name           text        NOT NULL,
    metadata       jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT auth_app_organizations_taap_unique UNIQUE (application_id, taap_id),
    CONSTRAINT auth_app_organizations_name_unique UNIQUE (application_id, name),
    CONSTRAINT auth_app_organizations_metadata_check CHECK (jsonb_typeof(metadata) = 'object')
);
DROP TRIGGER IF EXISTS auth_app_organizations_set_updated_at ON auth_app_organizations;
CREATE TRIGGER auth_app_organizations_set_updated_at BEFORE UPDATE ON auth_app_organizations
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS auth_app_webhooks (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id   uuid        NOT NULL REFERENCES auth_applications(id) ON DELETE CASCADE,
    url              text        NOT NULL,
    events           text[]      NOT NULL,
    encrypted_secret text        NOT NULL,
    enabled          boolean     NOT NULL DEFAULT true,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT auth_app_webhooks_events_check CHECK (cardinality(events) > 0)
);
CREATE INDEX IF NOT EXISTS auth_app_webhooks_application_idx
    ON auth_app_webhooks (application_id, created_at DESC);
DROP TRIGGER IF EXISTS auth_app_webhooks_set_updated_at ON auth_app_webhooks;
CREATE TRIGGER auth_app_webhooks_set_updated_at BEFORE UPDATE ON auth_app_webhooks
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS auth_app_webhook_events (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id uuid        NOT NULL REFERENCES auth_applications(id) ON DELETE CASCADE,
    event          text        NOT NULL,
    payload        jsonb       NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT auth_app_webhook_events_payload_check CHECK (jsonb_typeof(payload) = 'object')
);
CREATE INDEX IF NOT EXISTS auth_app_webhook_events_application_idx
    ON auth_app_webhook_events (application_id, created_at DESC);

CREATE TABLE IF NOT EXISTS auth_app_webhook_deliveries (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id   uuid        NOT NULL REFERENCES auth_app_webhook_events(id) ON DELETE CASCADE,
    webhook_id uuid        NOT NULL REFERENCES auth_app_webhooks(id) ON DELETE CASCADE,
    attempt    integer     NOT NULL CHECK (attempt > 0),
    status     integer     NOT NULL DEFAULT 0,
    signature  text        NOT NULL,
    error      text        NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT auth_app_webhook_deliveries_attempt_unique UNIQUE (event_id, webhook_id, attempt)
);
CREATE INDEX IF NOT EXISTS auth_app_webhook_deliveries_event_idx
    ON auth_app_webhook_deliveries (event_id, created_at);

CREATE TABLE IF NOT EXISTS auth_app_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES ('0016_auth_apps') ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- Agent sessions (migration 0017): short-lived per-session signing identities delegated to a
-- durable agent identity. Private key material is returned once and never enters this table.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS agent_sessions (
    id                       uuid        PRIMARY KEY,
    customer_id              uuid        NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    durable_agent_taap_id    text        NOT NULL,
    dispatcher_taap_id       text        NOT NULL,
    session_taap_id          text        NOT NULL UNIQUE,
    public_key               text        NOT NULL,
    scopes                   text[]      NOT NULL,
    attestation              text        NOT NULL,
    transparency_index       bigint      NOT NULL CHECK (transparency_index >= 0),
    transparency_root        text        NOT NULL,
    issued_at                timestamptz NOT NULL,
    expires_at               timestamptz NOT NULL,
    CONSTRAINT agent_sessions_scopes_check CHECK (cardinality(scopes) > 0),
    CONSTRAINT agent_sessions_expiry_check CHECK (expires_at > issued_at)
);
CREATE INDEX IF NOT EXISTS agent_sessions_customer_agent_idx
    ON agent_sessions (customer_id, durable_agent_taap_id, issued_at DESC);

CREATE TABLE IF NOT EXISTS agent_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES ('0017_agent_sessions') ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- Settings mutations (migration 0018): customer-specific storage quota overrides.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS customer_storage_quotas (
    customer_id uuid        PRIMARY KEY REFERENCES customers(id) ON DELETE CASCADE,
    quota_bytes bigint      NOT NULL CHECK (quota_bytes >= 0),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS settings_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES ('0018_settings_surface') ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- Platform mirror (migration 0019): repository environments and the last operator-control rows
-- Lake observed. Zetta remains the append-only deployment authority; this mirror keeps pages
-- available during an operator-control outage.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS platform_environments (
    id            uuid        PRIMARY KEY,
    repository_id uuid        NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    name          text        NOT NULL,
    object_id     uuid        NOT NULL UNIQUE REFERENCES objects(id) ON DELETE CASCADE,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT platform_environments_repository_name_unique UNIQUE (repository_id, name),
    CONSTRAINT platform_environments_name_check CHECK (length(name) BETWEEN 1 AND 256)
);
CREATE INDEX IF NOT EXISTS platform_environments_repository_idx
    ON platform_environments (repository_id, name);

CREATE TABLE IF NOT EXISTS platform_mirror (
    kind    text        NOT NULL,
    key     text        NOT NULL,
    value   jsonb       NOT NULL,
    seen_at timestamptz NOT NULL,
    PRIMARY KEY (kind, key),
    CONSTRAINT platform_mirror_kind_check
        CHECK (kind IN ('deployment', 'environment_env', 'domain')),
    CONSTRAINT platform_mirror_value_check CHECK (jsonb_typeof(value) = 'object')
);

CREATE TABLE IF NOT EXISTS platform_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES ('0019_platform') ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- Identity, PKI, and dynamic-secret exit scenario (migration 0020): replay-resistant TAAP
-- sign-in assertions, one-use OIDC authorization codes, certificate status, and renewable leases.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS identity_taap_assertions (
    assertion_id text        PRIMARY KEY,
    user_id      uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    pool_id      text        NOT NULL,
    used_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS auth_app_authorization_codes (
    code_hash      text        PRIMARY KEY,
    application_id uuid        NOT NULL REFERENCES auth_applications(id) ON DELETE CASCADE,
    user_id        uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    redirect_uri   text        NOT NULL,
    code_challenge text        NOT NULL,
    nonce          text        NOT NULL,
    expires_at     timestamptz NOT NULL DEFAULT now() + interval '5 minutes',
    used_at        timestamptz
);
CREATE INDEX IF NOT EXISTS auth_app_authorization_codes_live_idx
    ON auth_app_authorization_codes (application_id, expires_at) WHERE used_at IS NULL;

ALTER TABLE pki_certificates ADD COLUMN IF NOT EXISTS revoked_at timestamptz;
ALTER TABLE pki_certificates DROP CONSTRAINT IF EXISTS pki_certificates_profile_check;
ALTER TABLE pki_certificates ADD CONSTRAINT pki_certificates_profile_check
    CHECK (profile IN ('x509_svid', 'jwt_svid', 'acme', 'ssh', 'client', 'device', 'agent'));
ALTER TABLE sessions DROP CONSTRAINT IF EXISTS sessions_kind_check;
ALTER TABLE sessions ADD CONSTRAINT sessions_kind_check
    CHECK (kind IN ('browser', 'api_token', 'static', 'admin', 'taap'));

CREATE TABLE IF NOT EXISTS secrets_dynamic_leases (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    mount_id        uuid        NOT NULL REFERENCES secrets_mounts(id) ON DELETE CASCADE,
    role            text        NOT NULL,
    encrypted_value text        NOT NULL,
    expires_at      timestamptz NOT NULL,
    revoked_at      timestamptz,
    renewals        integer     NOT NULL DEFAULT 0,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT secrets_dynamic_leases_role_check CHECK (length(role) BETWEEN 1 AND 256),
    CONSTRAINT secrets_dynamic_leases_expiry_check CHECK (expires_at > created_at),
    CONSTRAINT secrets_dynamic_leases_renewals_check CHECK (renewals >= 0)
);
CREATE INDEX IF NOT EXISTS secrets_dynamic_leases_mount_idx
    ON secrets_dynamic_leases (mount_id, created_at DESC);

CREATE TABLE IF NOT EXISTS identity_exit_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES ('0020_identity_exit') ON CONFLICT DO NOTHING;

-- Token attenuation (migration 0021): retain issuing sessions and invalidate credentials
-- whose ancestry predates this contract. Reapplying the schema does not revoke new tokens.
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS parent_session_id uuid
    REFERENCES sessions(id) ON DELETE CASCADE;
CREATE INDEX IF NOT EXISTS sessions_parent_session_id_idx
    ON sessions (parent_session_id) WHERE parent_session_id IS NOT NULL;
UPDATE sessions SET revoked_at = now()
    WHERE kind IN ('api_token', 'static') AND revoked_at IS NULL
      AND NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '0021_token_attenuation');
CREATE TABLE IF NOT EXISTS token_attenuation_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0021_token_attenuation') ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- Indexed session authentication (migration 0022). Legacy Argon2 credentials cannot be
-- converted without their bearer. Revoke them once; users sign in and replace their tokens.
-- ---------------------------------------------------------------------------------------------
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS lookup_id uuid;
CREATE UNIQUE INDEX IF NOT EXISTS sessions_lookup_id_idx ON sessions (lookup_id);

UPDATE sessions SET revoked_at = now()
WHERE lookup_id IS NULL AND revoked_at IS NULL
  AND NOT EXISTS (
      SELECT 1 FROM schema_migrations WHERE version = '0022_session_lookup'
  );

CREATE TABLE IF NOT EXISTS session_lookup_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES ('0022_session_lookup') ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- Custodial TAAP identities (migration 0023). A person who signs in through Clerk arrives with
-- no Liquid identity document, and policy is keyed on TAAP identities, so lake mints one on their
-- first sign-in and holds its Ed25519 seed wrapped under the instance key, as it holds its own
-- (lake_identity). See docs/api/clerk.md and liquid-bridge decision 44.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS custodial_identities (
    user_id      uuid        PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    taap_id      text        NOT NULL UNIQUE,
    public_key   text        NOT NULL,
    wrapped_seed text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS custodial_identity_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES ('0023_custodial_identities') ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- Managed services (migration 0024). A customer runs any number of services on the Lake's
-- cluster: Zetta clusters and Postgres clusters today. lake writes the desired state here; the
-- Orama operator reads it, creates one OramaZettaCluster or CloudNativePG Cluster per service,
-- and reports back in service_status. The customer's Design, Train and Ship workspace is its
-- default Zetta service. See docs/control-plane/services.md and liquid-bridge decision 46.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS services (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id        uuid        NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    kind               text        NOT NULL,
    name               text        NOT NULL,
    -- Kubernetes names are <lake>-<slug>; unique for the life of the Lake, never reused.
    slug               text        NOT NULL UNIQUE,
    is_default         boolean     NOT NULL DEFAULT false,
    spec               jsonb       NOT NULL DEFAULT '{}'::jsonb,
    desired            text        NOT NULL DEFAULT 'present',
    generation         bigint      NOT NULL DEFAULT 1,
    created_by_taap_id text        NOT NULL DEFAULT '',
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT service_kind_check CHECK (kind IN ('zetta', 'postgres')),
    CONSTRAINT service_name_format CHECK (name ~ '^[a-z][a-z0-9-]{0,30}$'),
    CONSTRAINT service_slug_format CHECK (slug ~ '^(svc|ws)-[0-9a-f]{12}$'),
    CONSTRAINT service_desired_check CHECK (desired IN ('present', 'absent')),
    CONSTRAINT service_spec_object CHECK (jsonb_typeof(spec) = 'object'),
    CONSTRAINT service_default_is_zetta CHECK (NOT is_default OR kind = 'zetta')
);

-- One live service per name, and one default workspace, per customer.
CREATE UNIQUE INDEX IF NOT EXISTS services_customer_name_idx
    ON services (customer_id, name) WHERE desired = 'present';
CREATE UNIQUE INDEX IF NOT EXISTS services_customer_default_idx
    ON services (customer_id) WHERE is_default AND desired = 'present';
CREATE INDEX IF NOT EXISTS services_customer_idx ON services (customer_id, created_at);

-- Written only by the operator: one row per service, replaced on every observed change.
CREATE TABLE IF NOT EXISTS service_status (
    service_id          uuid        PRIMARY KEY REFERENCES services(id) ON DELETE CASCADE,
    phase               text        NOT NULL DEFAULT '',
    message             text        NOT NULL DEFAULT '',
    endpoint            text        NOT NULL DEFAULT '',
    observed_generation bigint      NOT NULL DEFAULT 0,
    updated_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS services_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES ('0024_services') ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- The service catalog (migration 0025): Kafka, ClickHouse, TiDB, Cassandra and Valkey join Zetta
-- and Postgres, each run by its own upstream operator that the Orama operator drives
-- (docs/control-plane/services.md, liquid-bridge decision 47). Re-adding the constraint is
-- idempotent: every existing row already satisfies the wider list.
-- ---------------------------------------------------------------------------------------------
ALTER TABLE services DROP CONSTRAINT IF EXISTS service_kind_check;
ALTER TABLE services ADD CONSTRAINT service_kind_check
    CHECK (kind IN ('zetta', 'postgres', 'kafka', 'clickhouse', 'tidb', 'cassandra', 'valkey'));

CREATE TABLE IF NOT EXISTS service_catalog_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES ('0025_service_catalog') ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- SQLite services (migration 0026): a small web service over one SQLite database per service,
-- run by the Orama operator itself (docs/control-plane/services.md, liquid-bridge decision 47,
-- amended). Re-adding the constraint is idempotent.
-- ---------------------------------------------------------------------------------------------
ALTER TABLE services DROP CONSTRAINT IF EXISTS service_kind_check;
ALTER TABLE services ADD CONSTRAINT service_kind_check
    CHECK (kind IN ('zetta', 'postgres', 'kafka', 'clickhouse', 'tidb', 'cassandra', 'valkey',
                    'sqlite'));

CREATE TABLE IF NOT EXISTS sqlite_services_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES ('0026_sqlite_services') ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- DuckDB and Polars services (migration 0027): each behind a small wrapper service of ours that
-- the Orama operator runs, like lake-sqlite (docs/control-plane/services.md, liquid-bridge
-- decision 47, amended; lake#195). Re-adding the constraint is idempotent.
-- ---------------------------------------------------------------------------------------------
ALTER TABLE services DROP CONSTRAINT IF EXISTS service_kind_check;
ALTER TABLE services ADD CONSTRAINT service_kind_check
    CHECK (kind IN ('zetta', 'postgres', 'kafka', 'clickhouse', 'tidb', 'cassandra', 'valkey',
                    'sqlite', 'duckdb', 'polars'));

CREATE TABLE IF NOT EXISTS analytics_services_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES ('0027_analytics_services') ON CONFLICT DO NOTHING;
