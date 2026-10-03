-- lake control-plane schema.
--
-- SOURCE OF TRUTH: oramasearch/lake, db/schema.sql. The operator vendors a byte-identical copy at
-- internal/controller/assets/lake-schema.sql and applies it to each Lake's Postgres through a
-- PostgresSchema (one transaction, guarded on the `app_forward_auth_guard` table, versioned by
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
-- NOT VALID: the whole file is re-applied on every schema change, and rows of kinds a later
-- migration adds already exist then; only the newest migration's constraint validates rows.
ALTER TABLE services ADD CONSTRAINT service_kind_check
    CHECK (kind IN ('zetta', 'postgres', 'kafka', 'clickhouse', 'tidb', 'cassandra', 'valkey')) NOT VALID;

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
-- NOT VALID: the whole file is re-applied on every schema change, and rows of kinds a later
-- migration adds already exist then; only the newest migration's constraint validates rows.
ALTER TABLE services ADD CONSTRAINT service_kind_check
    CHECK (kind IN ('zetta', 'postgres', 'kafka', 'clickhouse', 'tidb', 'cassandra', 'valkey',
                    'sqlite')) NOT VALID;

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

-- ---------------------------------------------------------------------------------------------
-- Storage (migration 0028, lake#196): a customer's stores, one general model with two axes — the
-- type (object, filesystem, network_filesystem, volume) and the provider that runs it (cluster,
-- r2, s3, b2, azure, gcs, lake, zetta, nfs). lake writes the desired state here and NOTIFYs
-- lake_services; the Orama operator reconciles each store, writes store_status and the store's
-- connection to <slug>.json in the Lake's services Secret, as it does for services. See
-- docs/control-plane/storage.md.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS stores (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id        uuid        NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    type               text        NOT NULL,
    provider           text        NOT NULL,
    name               text        NOT NULL,
    -- Kubernetes names are <lake>-<slug>; unique for the life of the Lake, never reused.
    slug               text        NOT NULL UNIQUE,
    -- {size_gi, region, public}; public is false unless the customer chose otherwise.
    spec               jsonb       NOT NULL DEFAULT '{}'::jsonb,
    desired            text        NOT NULL DEFAULT 'present',
    generation         bigint      NOT NULL DEFAULT 1,
    created_by_taap_id text        NOT NULL DEFAULT '',
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT store_type_check
        CHECK (type IN ('object', 'filesystem', 'network_filesystem', 'volume')),
    CONSTRAINT store_provider_check
        CHECK (provider IN ('cluster', 'r2', 's3', 'b2', 'azure', 'gcs', 'lake', 'zetta', 'nfs')),
    CONSTRAINT store_name_format CHECK (name ~ '^[a-z][a-z0-9-]{0,30}$'),
    CONSTRAINT store_slug_format CHECK (slug ~ '^st-[0-9a-f]{12}$'),
    CONSTRAINT store_desired_check CHECK (desired IN ('present', 'absent')),
    CONSTRAINT store_spec_object CHECK (jsonb_typeof(spec) = 'object')
);

-- One live store per name per customer.
CREATE UNIQUE INDEX IF NOT EXISTS stores_customer_name_idx
    ON stores (customer_id, name) WHERE desired = 'present';
CREATE INDEX IF NOT EXISTS stores_customer_idx ON stores (customer_id, created_at);

-- Written only by the operator: one row per store, replaced on every observed change.
CREATE TABLE IF NOT EXISTS store_status (
    store_id            uuid        PRIMARY KEY REFERENCES stores(id) ON DELETE CASCADE,
    phase               text        NOT NULL DEFAULT '',
    message             text        NOT NULL DEFAULT '',
    endpoint            text        NOT NULL DEFAULT '',
    observed_generation bigint      NOT NULL DEFAULT 0,
    updated_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS stores_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES ('0028_stores') ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- Credentials that never expire (migration 0029, lake#234): a customer's opt-in, off by default,
-- and revocation for agent session keys. A never-expiring agent key stores expires_at =
-- 'infinity', which satisfies agent_sessions_expiry_check. See docs/api/agents.md and
-- docs/api/identity.md.
-- ---------------------------------------------------------------------------------------------
ALTER TABLE customers ADD COLUMN IF NOT EXISTS allow_non_expiring_credentials boolean NOT NULL DEFAULT false;
ALTER TABLE agent_sessions ADD COLUMN IF NOT EXISTS revoked_at timestamptz;

CREATE TABLE IF NOT EXISTS credentials_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES ('0029_non_expiring_credentials') ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- Agent launches, schedules and Ask Orama conversations (migration 0030, lake#231, lake#236).
-- lake writes agent_launches and NOTIFYs lake_services; the Orama operator runs each present
-- launch on agent-runtime (a Job for mode job, a Deployment and Service for mode service) and
-- writes agent_launch_status, including the SHA-256 of the credential it generated for the pod.
-- The credential itself is never stored. See docs/api/agent-launches.md.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS agent_launches (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id        uuid        NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    -- The AgentRecord in the customer's workspace Zetta.
    agent_id           text        NOT NULL,
    agent_name         text        NOT NULL DEFAULT '',
    mode               text        NOT NULL,
    input              jsonb       NOT NULL DEFAULT 'null'::jsonb,
    -- The lake capabilities the pod's credential may call.
    scopes             text[]      NOT NULL,
    slug               text        NOT NULL UNIQUE,
    desired            text        NOT NULL DEFAULT 'present',
    generation         bigint      NOT NULL DEFAULT 1,
    trigger            text        NOT NULL DEFAULT 'manual',
    created_by_user_id uuid        REFERENCES users(id) ON DELETE CASCADE,
    created_by_taap_id text        NOT NULL DEFAULT '',
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT agent_launch_mode_check CHECK (mode IN ('job', 'service')),
    CONSTRAINT agent_launch_desired_check CHECK (desired IN ('present', 'absent')),
    CONSTRAINT agent_launch_slug_format CHECK (slug ~ '^al-[0-9a-f]{12}$'),
    CONSTRAINT agent_launch_scopes_check CHECK (cardinality(scopes) > 0)
);
CREATE INDEX IF NOT EXISTS agent_launches_customer_agent_idx
    ON agent_launches (customer_id, agent_id, created_at DESC);

-- Written only by the operator: one row per launch, replaced on every observed change.
CREATE TABLE IF NOT EXISTS agent_launch_status (
    launch_id           uuid        PRIMARY KEY REFERENCES agent_launches(id) ON DELETE CASCADE,
    phase               text        NOT NULL DEFAULT '',
    message             text        NOT NULL DEFAULT '',
    endpoint            text        NOT NULL DEFAULT '',
    exit_code           integer,
    result              jsonb       NOT NULL DEFAULT 'null'::jsonb,
    credential_sha256   text        NOT NULL DEFAULT '',
    observed_generation bigint      NOT NULL DEFAULT 0,
    updated_at          timestamptz NOT NULL DEFAULT now()
);

-- Retired by 0031_agent_cron_schedules (lake no longer fires schedules itself); kept, unread, until
-- its rows are dropped deliberately.
CREATE TABLE IF NOT EXISTS agent_schedules (
    customer_id        uuid        NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    agent_id           text        NOT NULL,
    agent_name         text        NOT NULL DEFAULT '',
    trigger            text        NOT NULL,
    next_run_at        timestamptz NOT NULL,
    last_run_at        timestamptz,
    created_by_user_id uuid        REFERENCES users(id) ON DELETE CASCADE,
    created_by_taap_id text        NOT NULL DEFAULT '',
    PRIMARY KEY (customer_id, agent_id)
);
CREATE INDEX IF NOT EXISTS agent_schedules_due_idx ON agent_schedules (next_run_at);

-- Ask Orama conversations: rendered by whichever replica serves the panel.
CREATE TABLE IF NOT EXISTS ask_messages (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id uuid        NOT NULL,
    customer_id     uuid        REFERENCES customers(id) ON DELETE CASCADE,
    user_id         uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role            text        NOT NULL,
    content         text        NOT NULL DEFAULT '',
    payload         jsonb       NOT NULL DEFAULT 'null'::jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT ask_message_role_check CHECK (role IN ('user', 'assistant'))
);
CREATE INDEX IF NOT EXISTS ask_messages_conversation_idx
    ON ask_messages (conversation_id, created_at);

CREATE TABLE IF NOT EXISTS agents_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES ('0030_agent_launches') ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0031_agent_cron_schedules: a Python agent's cron schedules, any number per agent. lake writes
-- the desired schedule; the Orama operator makes each present one a Kubernetes CronJob (named
-- after the slug) and writes agent_cron_schedule_status and one agent_cron_runs row per Job the
-- CronJob starts. Every run authenticates with the schedule's credential,
-- las.<schedule id>.<secret>, which the operator generates and of which it writes only the SHA-256.
-- See docs/api/agent-launches.md.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS agent_cron_schedules (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id        uuid        NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    agent_id           text        NOT NULL,
    agent_name         text        NOT NULL DEFAULT '',
    -- Five fields, as a Kubernetes CronJob's spec.schedule; lake validates it.
    cron               text        NOT NULL,
    time_zone          text        NOT NULL DEFAULT 'Etc/UTC',
    -- The plain words it was set from, when it was ("every monday at 09:00").
    description        text        NOT NULL DEFAULT '',
    input              jsonb       NOT NULL DEFAULT 'null'::jsonb,
    scopes             text[]      NOT NULL,
    slug               text        NOT NULL UNIQUE,
    desired            text        NOT NULL DEFAULT 'present',
    generation         bigint      NOT NULL DEFAULT 1,
    created_by_user_id uuid        REFERENCES users(id) ON DELETE CASCADE,
    created_by_taap_id text        NOT NULL DEFAULT '',
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT agent_cron_schedule_desired_check CHECK (desired IN ('present', 'absent')),
    CONSTRAINT agent_cron_schedule_slug_format CHECK (slug ~ '^as-[0-9a-f]{12}$'),
    CONSTRAINT agent_cron_schedule_scopes_check CHECK (cardinality(scopes) > 0)
);
CREATE INDEX IF NOT EXISTS agent_cron_schedules_customer_agent_idx
    ON agent_cron_schedules (customer_id, agent_id, created_at);

-- Written only by the operator: one row per schedule, replaced on every observed change.
CREATE TABLE IF NOT EXISTS agent_cron_schedule_status (
    schedule_id         uuid        PRIMARY KEY REFERENCES agent_cron_schedules(id) ON DELETE CASCADE,
    phase               text        NOT NULL DEFAULT '',
    message             text        NOT NULL DEFAULT '',
    last_schedule_at    timestamptz,
    credential_sha256   text        NOT NULL DEFAULT '',
    observed_generation bigint      NOT NULL DEFAULT 0,
    updated_at          timestamptz NOT NULL DEFAULT now()
);

-- Written only by the operator: one row per Job a schedule's CronJob started, keyed by the Job's uid.
CREATE TABLE IF NOT EXISTS agent_cron_runs (
    id          uuid        PRIMARY KEY,
    schedule_id uuid        NOT NULL REFERENCES agent_cron_schedules(id) ON DELETE CASCADE,
    job_name    text        NOT NULL,
    phase       text        NOT NULL DEFAULT '',
    message     text        NOT NULL DEFAULT '',
    exit_code   integer,
    result      jsonb       NOT NULL DEFAULT 'null'::jsonb,
    started_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS agent_cron_runs_schedule_idx
    ON agent_cron_runs (schedule_id, started_at DESC);

CREATE TABLE IF NOT EXISTS agent_cron_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES ('0031_agent_cron_schedules') ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0032_agent_tasks: agent turns run in the background (lake#269, docs/api/agent-bus.md). lake
-- writes the turn, publishes it on the agent bus (Zetta pub/sub, topic agent-tasks.<agent>) and
-- answers the HTTP request at once. The agent claims it through lake, which hands it a fresh
-- credential (lat.<task id>.<secret>, only its SHA-256 stored); what the agent publishes on
-- agent-events lake writes into agent_task_status. No page or capability waits on an agent.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS agent_tasks (
    id                   uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id          uuid        NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    -- The agent of the Agent Library the turn goes to (agent-ask-orama, agent-project, …).
    agent                text        NOT NULL,
    -- What asked: ask, factory, discover, pipeline or api; and the conversation it belongs to.
    kind                 text        NOT NULL DEFAULT 'api',
    conversation_id      uuid,
    -- The body the agent receives when it claims the task; "{{credential}}" is replaced by the
    -- task's credential, which lake hands over once and never stores.
    body                 jsonb       NOT NULL DEFAULT '{}'::jsonb,
    -- The lake capabilities the task's credential may call.
    scopes               text[]      NOT NULL,
    deadline_seconds     integer     NOT NULL DEFAULT 300,
    desired              text        NOT NULL DEFAULT 'present',
    generation           bigint      NOT NULL DEFAULT 1,
    requested_by_user_id uuid        REFERENCES users(id) ON DELETE CASCADE,
    requested_by_taap_id text        NOT NULL DEFAULT '',
    -- When lake last published it on the bus; an unclaimed task is published again.
    published_at         timestamptz,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT agent_task_desired_check CHECK (desired IN ('present', 'absent')),
    CONSTRAINT agent_task_scopes_check CHECK (cardinality(scopes) > 0)
);
CREATE INDEX IF NOT EXISTS agent_tasks_conversation_idx
    ON agent_tasks (conversation_id, created_at);
CREATE INDEX IF NOT EXISTS agent_tasks_customer_idx ON agent_tasks (customer_id, created_at DESC);

-- Written by lake: inserted when the agent claims the task, updated by what it publishes.
CREATE TABLE IF NOT EXISTS agent_task_status (
    task_id             uuid        PRIMARY KEY REFERENCES agent_tasks(id) ON DELETE CASCADE,
    phase               text        NOT NULL DEFAULT '',
    message             text        NOT NULL DEFAULT '',
    result              jsonb       NOT NULL DEFAULT 'null'::jsonb,
    credential_sha256   text        NOT NULL DEFAULT '',
    started_at          timestamptz,
    finished_at         timestamptz,
    observed_generation bigint      NOT NULL DEFAULT 0,
    updated_at          timestamptz NOT NULL DEFAULT now()
);

-- Design → Opportunities (lake#269): what Auto-Suggest recommended, kept across scans. A scan task
-- that finished writes its rows once; a person turns them into projects or dismisses them.
CREATE TABLE IF NOT EXISTS opportunities (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id        uuid        NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    task_id            uuid        NOT NULL REFERENCES agent_tasks(id) ON DELETE CASCADE,
    rank               integer     NOT NULL DEFAULT 0,
    intent             text        NOT NULL,
    solution           text        NOT NULL DEFAULT '',
    methodology        text        NOT NULL DEFAULT '',
    why                text        NOT NULL DEFAULT '',
    payload            jsonb       NOT NULL DEFAULT '{}'::jsonb,
    -- new, project (made into a project), dismissed, or not_yet (Auto-Suggest declined it)
    status             text        NOT NULL DEFAULT 'new',
    project            text        NOT NULL DEFAULT '',
    created_by_user_id uuid        REFERENCES users(id) ON DELETE SET NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT opportunity_status_check CHECK (status IN ('new', 'project', 'dismissed', 'not_yet'))
);
CREATE INDEX IF NOT EXISTS opportunities_customer_idx ON opportunities (customer_id, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS opportunities_task_rank_idx ON opportunities (task_id, rank, status);

CREATE TABLE IF NOT EXISTS agent_tasks_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES ('0032_agent_tasks') ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0033_agent_task_events: where a task's events start on the agent bus (lake#275). When an agent
-- claims a task, lake records the agent-events sequence its bus loop has reached: every event the
-- task publishes comes after it, so following the task reads from there instead of from the start
-- of a topic that only grows. 0 means unknown (read from the start).
-- ---------------------------------------------------------------------------------------------
ALTER TABLE agent_task_status ADD COLUMN IF NOT EXISTS events_from bigint NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS agent_task_events_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES ('0033_agent_task_events') ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0034_web_apps: Ship apps desired state and the status the Orama operator observes (lake#293).
-- lake writes the desired fields, increments generation, and NOTIFYs lake_apps. The operator
-- reconciles one LakeApp per row and writes phase through observed_generation back on this row.
-- The operator writes token hashes; bindings contain read scopes and service IDs only.
-- env_json never contains clear credentials. See docs/control-plane/web-apps.md.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS apps (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id         uuid        NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    name                text        NOT NULL,
    mode                text        NOT NULL,
    source_repository   text        NOT NULL DEFAULT '',
    source_revision     text        NOT NULL DEFAULT '',
    source_path         text        NOT NULL DEFAULT '',
    source_entrypoint   text        NOT NULL DEFAULT '',
    image               text        NOT NULL DEFAULT '',
    framework           text        NOT NULL DEFAULT '',
    port                integer     NOT NULL,
    replicas            integer     NOT NULL DEFAULT 1,
    host                text        NOT NULL,
    auth                text        NOT NULL DEFAULT 'lake',
    bindings_json       jsonb       NOT NULL DEFAULT '{"zetta":null,"services":[]}'::jsonb,
    env_json            jsonb       NOT NULL DEFAULT '{}'::jsonb,
    secret_env_refs     jsonb       NOT NULL DEFAULT '{}'::jsonb,
    token_hashes_json   jsonb       NOT NULL DEFAULT '{}'::jsonb,
    desired             text        NOT NULL DEFAULT 'running',
    generation          bigint      NOT NULL DEFAULT 1,
    phase               text        NOT NULL DEFAULT 'Pending',
    url                 text        NOT NULL DEFAULT '',
    ready               boolean     NOT NULL DEFAULT false,
    message             text        NOT NULL DEFAULT '',
    observed_generation bigint      NOT NULL DEFAULT 0,
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT app_name_format CHECK (name ~ '^[a-z]([a-z0-9-]{0,29}[a-z0-9])?$'),
    CONSTRAINT app_mode_check CHECK (mode IN ('runtime', 'image')),
    CONSTRAINT app_source_or_image_check CHECK (
        (mode = 'runtime' AND source_repository <> '' AND source_revision <> '' AND image = '') OR
        (mode = 'image' AND image <> '' AND source_repository = '' AND source_revision = '')
    ),
    CONSTRAINT app_port_check CHECK (port BETWEEN 1 AND 65535),
    CONSTRAINT app_replicas_check CHECK (replicas BETWEEN 1 AND 100),
    CONSTRAINT app_auth_check CHECK (auth IN ('lake', 'none')),
    CONSTRAINT app_desired_check CHECK (desired IN ('running', 'stopped', 'deleted')),
    CONSTRAINT app_bindings_object CHECK (jsonb_typeof(bindings_json) = 'object'),
    CONSTRAINT app_env_object CHECK (jsonb_typeof(env_json) = 'object'),
    CONSTRAINT app_secret_refs_object CHECK (jsonb_typeof(secret_env_refs) = 'object')
);

CREATE UNIQUE INDEX IF NOT EXISTS apps_customer_name_idx
    ON apps (customer_id, name) WHERE desired <> 'deleted';
CREATE UNIQUE INDEX IF NOT EXISTS apps_host_idx ON apps (host) WHERE desired <> 'deleted';
CREATE INDEX IF NOT EXISTS apps_customer_idx ON apps (customer_id, updated_at);

CREATE TABLE IF NOT EXISTS web_apps_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES ('0034_web_apps') ON CONFLICT DO NOTHING;

-- 0035_web_apps_operator_contract: upgrade earlier Apps rows to the operator-owned token contract.
-- This also works after 0034 has already been applied. No app bearer material is retained by lake.
ALTER TABLE apps ADD COLUMN IF NOT EXISTS source_entrypoint text NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN IF NOT EXISTS token_hashes_json jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE apps DROP CONSTRAINT IF EXISTS app_mode_check;
ALTER TABLE apps DROP CONSTRAINT IF EXISTS app_source_or_image_check;
ALTER TABLE apps DROP CONSTRAINT IF EXISTS app_desired_check;
-- Earlier partial indexes exclude absent rows. Remove them before converting historical rows,
-- which may legitimately reuse a name or host after deletion.
DROP INDEX IF EXISTS apps_customer_name_idx;
DROP INDEX IF EXISTS apps_host_idx;
UPDATE apps SET mode = 'runtime' WHERE mode = 'source';
UPDATE apps SET desired = 'deleted' WHERE desired = 'absent';
UPDATE apps SET phase = 'Running' WHERE phase = 'Ready';
ALTER TABLE apps ALTER COLUMN phase SET DEFAULT 'Pending';
ALTER TABLE apps ALTER COLUMN bindings_json SET DEFAULT '{"zetta":null,"services":[]}'::jsonb;
ALTER TABLE apps ADD CONSTRAINT app_mode_check CHECK (mode IN ('runtime', 'image'));
ALTER TABLE apps ADD CONSTRAINT app_source_or_image_check CHECK (
    (mode = 'runtime' AND source_repository <> '' AND source_revision <> '' AND image = '') OR
    (mode = 'image' AND image <> '' AND source_repository = '' AND source_revision = '')
);
ALTER TABLE apps ADD CONSTRAINT app_desired_check CHECK (desired IN ('running', 'stopped', 'deleted'));
UPDATE apps SET bindings_json = jsonb_build_object(
    'zetta', CASE WHEN jsonb_typeof(bindings_json->'zetta') = 'object'
        OR bindings_json->'zetta' = 'true'::jsonb
        THEN '{"scopes":["read"]}'::jsonb ELSE 'null'::jsonb END,
    'services', COALESCE((SELECT jsonb_agg(CASE WHEN jsonb_typeof(value) = 'object'
        THEN value->'id' ELSE value END)
        FROM jsonb_array_elements(COALESCE(bindings_json->'services', '[]'::jsonb))), '[]'::jsonb)
) WHERE NOT EXISTS (
    SELECT 1 FROM schema_migrations WHERE version = '0035_web_apps_operator_contract'
);
CREATE UNIQUE INDEX apps_customer_name_idx ON apps (customer_id, name) WHERE desired <> 'deleted';
CREATE UNIQUE INDEX apps_host_idx ON apps (host) WHERE desired <> 'deleted';
CREATE INDEX IF NOT EXISTS apps_source_token_idx ON apps ((token_hashes_json->>'source'));
CREATE INDEX IF NOT EXISTS apps_zetta_token_idx ON apps ((token_hashes_json->>'zetta'));
CREATE INDEX IF NOT EXISTS apps_events_token_idx ON apps ((token_hashes_json->>'events'));
DROP TABLE IF EXISTS app_credentials;
CREATE TABLE IF NOT EXISTS web_apps_operator_contract_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0035_web_apps_operator_contract')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0036_builds: in-cluster image build intent and operator observation (lake#295). Lake writes
-- queued/cancelled desired state and NOTIFYs lake_builds. The operator owns phase, result and the
-- SHA-256 hashes of its source, push and event credentials. Plain credentials never enter this
-- database. See docs/control-plane/builds.md.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS builds (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id         uuid        NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    repository          text        NOT NULL,
    revision            text        NOT NULL,
    context             text        NOT NULL DEFAULT '.',
    dockerfile          text        NOT NULL DEFAULT 'Dockerfile',
    target              text        NOT NULL,
    build_args_json     jsonb       NOT NULL DEFAULT '{}'::jsonb,
    desired             text        NOT NULL DEFAULT 'queued',
    phase               text        NOT NULL DEFAULT 'Queued',
    digest              text        NOT NULL DEFAULT '',
    size                bigint      NOT NULL DEFAULT 0,
    message             text        NOT NULL DEFAULT '',
    token_hashes_json   jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT build_desired_check CHECK (desired IN ('queued', 'cancelled')),
    CONSTRAINT build_phase_check CHECK (
        phase IN ('Queued', 'Running', 'Succeeded', 'Failed', 'Cancelled')
    ),
    CONSTRAINT build_args_object CHECK (jsonb_typeof(build_args_json) = 'object'),
    CONSTRAINT build_token_hashes_object CHECK (jsonb_typeof(token_hashes_json) = 'object'),
    CONSTRAINT build_size_check CHECK (size >= 0)
);
CREATE INDEX IF NOT EXISTS builds_customer_idx ON builds (customer_id, created_at DESC);
CREATE INDEX IF NOT EXISTS builds_source_token_idx ON builds ((token_hashes_json->>'source'));
CREATE INDEX IF NOT EXISTS builds_push_token_idx ON builds ((token_hashes_json->>'push'));
CREATE INDEX IF NOT EXISTS builds_events_token_idx ON builds ((token_hashes_json->>'events'));

CREATE TABLE IF NOT EXISTS builds_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES ('0036_builds') ON CONFLICT DO NOTHING;

-- 0037_streams: creator ownership survives audit retention and workspace replacement (lake#301).
CREATE TABLE IF NOT EXISTS stream_owners (
    customer_id uuid NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    workspace_id text NOT NULL,
    topic text NOT NULL,
    created_by text NOT NULL,
    PRIMARY KEY (customer_id, workspace_id, topic)
);
CREATE TABLE IF NOT EXISTS streams_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0037_streams') ON CONFLICT DO NOTHING;

-- 0038_workspace_data_plane: immutable routing, never event payloads (lake#311).
-- Legacy assignments are recorded, including rows requiring explicit remediation.
CREATE TABLE IF NOT EXISTS workspace_assignments (
    object_table text NOT NULL,
    object_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    workspace_id uuid,
    outcome text NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (object_table, object_id)
);

ALTER TABLE agent_tasks ADD COLUMN IF NOT EXISTS workspace_id uuid REFERENCES services(id);
ALTER TABLE apps ADD COLUMN IF NOT EXISTS workspace_id uuid REFERENCES services(id);
ALTER TABLE builds ADD COLUMN IF NOT EXISTS workspace_id uuid REFERENCES services(id);

DO $$
DECLARE
    target_table text;
    unresolved record;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '0038_workspace_data_plane') THEN
        FOREACH target_table IN ARRAY ARRAY['agent_tasks', 'apps', 'builds'] LOOP
            EXECUTE format(
                'INSERT INTO workspace_assignments (object_table, object_id, customer_id, workspace_id, outcome)
                 SELECT %L, o.id, o.customer_id, w.id,
                        CASE WHEN w.id IS NULL THEN ''needs_selection'' ELSE ''sole_default'' END
                 FROM %I o LEFT JOIN services w ON w.customer_id = o.customer_id
                      AND w.kind = ''zetta'' AND w.desired = ''present'' AND w.is_default
                      AND (SELECT count(*) FROM services s WHERE s.customer_id = o.customer_id
                           AND s.kind = ''zetta'' AND s.desired = ''present'') = 1
                 WHERE o.workspace_id IS NULL ON CONFLICT DO NOTHING', target_table, target_table);
            EXECUTE format(
                'UPDATE %I o SET workspace_id = a.workspace_id FROM workspace_assignments a
                 WHERE a.object_table = %L AND a.object_id = o.id
                       AND o.workspace_id IS NULL AND a.workspace_id IS NOT NULL',
                 target_table, target_table);
        END LOOP;
        FOR unresolved IN SELECT * FROM workspace_assignments WHERE workspace_id IS NULL LOOP
            RAISE WARNING 'workspace_id requires selection: table=%, object=%, customer=%',
                unresolved.object_table, unresolved.object_id, unresolved.customer_id;
        END LOOP;
    END IF;
END $$;

CREATE OR REPLACE FUNCTION enforce_workspace_routing() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF NEW.workspace_id IS DISTINCT FROM OLD.workspace_id OR
           NEW.customer_id IS DISTINCT FROM OLD.customer_id THEN
            RAISE EXCEPTION 'workspace_id and customer_id are immutable' USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.workspace_id IS NULL OR NOT EXISTS (
        SELECT 1 FROM services WHERE id = NEW.workspace_id AND customer_id = NEW.customer_id
            AND kind = 'zetta' AND desired = 'present'
    ) THEN
        RAISE EXCEPTION 'workspace_id must name a workspace Zetta in this space'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

DROP TRIGGER IF EXISTS agent_task_workspace_routing ON agent_tasks;
CREATE TRIGGER agent_task_workspace_routing BEFORE INSERT OR UPDATE ON agent_tasks
    FOR EACH ROW EXECUTE FUNCTION enforce_workspace_routing();
DROP TRIGGER IF EXISTS app_workspace_routing ON apps;
CREATE TRIGGER app_workspace_routing BEFORE INSERT OR UPDATE ON apps
    FOR EACH ROW EXECUTE FUNCTION enforce_workspace_routing();
DROP TRIGGER IF EXISTS build_workspace_routing ON builds;
CREATE TRIGGER build_workspace_routing BEFORE INSERT OR UPDATE ON builds
    FOR EACH ROW EXECUTE FUNCTION enforce_workspace_routing();

CREATE TABLE IF NOT EXISTS workspace_data_plane_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0038_workspace_data_plane') ON CONFLICT DO NOTHING;

-- 0039_workspaces: lake#265 / lake#311 operator contract. Lake owns desired state;
-- the operator owns phase, observed_generation, message and namespace.
CREATE TABLE IF NOT EXISTS workspaces (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id uuid NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 128),
    environment text NOT NULL CHECK (
        length(environment) <= 47 AND environment ~ '^[a-z0-9]([a-z0-9-]*[a-z0-9])?$'
    ),
    desired text NOT NULL DEFAULT 'running' CHECK (desired IN ('running', 'stopped', 'deleted')),
    generation bigint NOT NULL DEFAULT 1 CHECK (generation > 0),
    phase text NOT NULL DEFAULT 'Pending',
    observed_generation bigint NOT NULL DEFAULT 0,
    message text NOT NULL DEFAULT '',
    namespace text NOT NULL DEFAULT '',
    zetta_service_id uuid REFERENCES services(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS workspaces_customer_idx ON workspaces (customer_id, created_at, id);
CREATE UNIQUE INDEX IF NOT EXISTS workspaces_name_idx ON workspaces (customer_id, name)
    WHERE desired <> 'deleted';
CREATE UNIQUE INDEX IF NOT EXISTS workspaces_zetta_idx ON workspaces (zetta_service_id);
ALTER TABLE services ADD COLUMN IF NOT EXISTS workspace_id uuid REFERENCES workspaces(id);
ALTER TABLE agent_tasks DROP CONSTRAINT IF EXISTS agent_tasks_workspace_id_fkey;
ALTER TABLE apps DROP CONSTRAINT IF EXISTS apps_workspace_id_fkey;
ALTER TABLE builds DROP CONSTRAINT IF EXISTS builds_workspace_id_fkey;

CREATE OR REPLACE FUNCTION notify_lake_workspaces() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        PERFORM pg_notify('lake_workspaces', OLD.id::text);
        RETURN OLD;
    END IF;
    PERFORM pg_notify('lake_workspaces', NEW.id::text);
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS lake_workspaces_changed ON workspaces;
CREATE TRIGGER lake_workspaces_changed AFTER INSERT OR UPDATE OR DELETE ON workspaces
    FOR EACH ROW EXECUTE FUNCTION notify_lake_workspaces();

DO $$
DECLARE
    unresolved record;
    target_table text;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '0039_workspaces') THEN
        -- Match the operator's legacy name for the original customer-named workspace.
        INSERT INTO workspaces (id, customer_id, name, environment, desired, zetta_service_id,
                                created_at, updated_at)
            SELECT CASE WHEN slug = 'ws-' || left(replace(customer_id::text, '-', ''), 12)
                        THEN customer_id ELSE id END,
                   customer_id, name, 'prod',
                   CASE WHEN desired = 'absent' THEN 'deleted' ELSE 'running' END,
                   id, created_at, updated_at
            FROM services WHERE kind = 'zetta';
        UPDATE services s SET workspace_id = w.id,
            slug = 'ws-' || left(replace(w.id::text, '-', ''), 12)
            FROM workspaces w WHERE w.zetta_service_id = s.id;
        ALTER TABLE agent_tasks DISABLE TRIGGER agent_task_workspace_routing;
        ALTER TABLE apps DISABLE TRIGGER app_workspace_routing;
        ALTER TABLE builds DISABLE TRIGGER build_workspace_routing;
        FOREACH target_table IN ARRAY ARRAY['agent_tasks', 'apps', 'builds'] LOOP
            EXECUTE format(
                'INSERT INTO workspace_assignments (object_table, object_id, customer_id, workspace_id, outcome)
                 SELECT %L, o.id, o.customer_id, w.id, ''workspace_contract_remap''
                 FROM %I o JOIN workspaces w ON o.workspace_id = w.zetta_service_id
                 ON CONFLICT DO NOTHING', target_table, target_table);
        END LOOP;
        UPDATE agent_tasks o SET workspace_id = w.id FROM workspaces w
            WHERE o.workspace_id = w.zetta_service_id;
        UPDATE apps o SET workspace_id = w.id FROM workspaces w
            WHERE o.workspace_id = w.zetta_service_id;
        UPDATE builds o SET workspace_id = w.id FROM workspaces w
            WHERE o.workspace_id = w.zetta_service_id;
        UPDATE workspace_assignments a SET workspace_id = w.id FROM workspaces w
            WHERE a.workspace_id = w.zetta_service_id;
        ALTER TABLE agent_tasks ENABLE TRIGGER agent_task_workspace_routing;
        ALTER TABLE apps ENABLE TRIGGER app_workspace_routing;
        ALTER TABLE builds ENABLE TRIGGER build_workspace_routing;
        INSERT INTO workspace_assignments (object_table, object_id, customer_id, workspace_id, outcome)
            SELECT 'services', id, customer_id, workspace_id, 'workspace_contract_prod'
            FROM services WHERE kind = 'zetta' ON CONFLICT DO NOTHING;
        -- Other services can be assigned only where ownership is unambiguous.
        UPDATE services s SET workspace_id = w.id FROM workspaces w
            WHERE s.workspace_id IS NULL AND s.customer_id = w.customer_id
              AND w.desired <> 'deleted'
              AND (SELECT count(*) FROM workspaces w2 WHERE w2.customer_id = s.customer_id
                   AND w2.desired <> 'deleted') = 1;
        INSERT INTO workspace_assignments (object_table, object_id, customer_id, workspace_id, outcome)
            SELECT 'services', id, customer_id, workspace_id,
                   CASE WHEN workspace_id IS NULL THEN 'needs_selection' ELSE 'sole_workspace' END
            FROM services WHERE kind <> 'zetta' ON CONFLICT DO NOTHING;
        FOR unresolved IN SELECT * FROM workspace_assignments WHERE workspace_id IS NULL LOOP
            RAISE WARNING 'workspace_id requires selection: table=%, object=%, customer=%',
                unresolved.object_table, unresolved.object_id, unresolved.customer_id;
        END LOOP;
    END IF;
END $$;

ALTER TABLE agent_tasks DROP CONSTRAINT IF EXISTS agent_tasks_workspace_id_fkey;
ALTER TABLE agent_tasks ADD CONSTRAINT agent_tasks_workspace_id_fkey
    FOREIGN KEY (workspace_id) REFERENCES workspaces(id);
ALTER TABLE apps DROP CONSTRAINT IF EXISTS apps_workspace_id_fkey;
ALTER TABLE apps ADD CONSTRAINT apps_workspace_id_fkey
    FOREIGN KEY (workspace_id) REFERENCES workspaces(id);
ALTER TABLE builds DROP CONSTRAINT IF EXISTS builds_workspace_id_fkey;
ALTER TABLE builds ADD CONSTRAINT builds_workspace_id_fkey
    FOREIGN KEY (workspace_id) REFERENCES workspaces(id);

CREATE OR REPLACE FUNCTION enforce_workspace_routing() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF NEW.workspace_id IS DISTINCT FROM OLD.workspace_id OR
           NEW.customer_id IS DISTINCT FROM OLD.customer_id THEN
            RAISE EXCEPTION 'workspace_id and customer_id are immutable' USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.workspace_id IS NULL OR NOT EXISTS (
        SELECT 1 FROM workspaces WHERE id = NEW.workspace_id AND customer_id = NEW.customer_id
            AND desired <> 'deleted'
    ) THEN
        RAISE EXCEPTION 'workspace_id must name a workspace in this space' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

CREATE TABLE IF NOT EXISTS workspaces_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0039_workspaces') ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0040_active_repository_names: tombstoned repositories retain their historical names without
-- reserving those names. Apps, services, stores and workspaces already limit their name indexes
-- to live desired states.
-- ---------------------------------------------------------------------------------------------
ALTER TABLE repositories DROP CONSTRAINT IF EXISTS repository_name_unique;
CREATE UNIQUE INDEX IF NOT EXISTS repository_name_unique
    ON repositories (customer_id, name) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS repository_names_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0040_active_repository_names')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0041_app_forward_auth: short-lived, single-use handoff codes for Lake-protected apps. The
-- browser receives the random code; only its SHA-256 digest is retained as control-plane state.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS app_authorization_codes (
    code_hash  char(64)    PRIMARY KEY,
    app_id     uuid        NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    user_id    uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    host       text        NOT NULL,
    expires_at timestamptz NOT NULL DEFAULT now() + interval '60 seconds',
    used_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT app_authorization_codes_hash_check CHECK (code_hash ~ '^[0-9a-f]{64}$')
);
CREATE INDEX IF NOT EXISTS app_authorization_codes_expiry_idx
    ON app_authorization_codes (expires_at);
CREATE INDEX IF NOT EXISTS memberships_app_auth_idx
    ON memberships (organization_id, principal_type, principal_id);

CREATE TABLE IF NOT EXISTS app_forward_auth_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0041_app_forward_auth')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0042_agent_task_leases: renewable claims and bounded retry after a claimant stops responding.
-- Only claim/dispatch state lives here; retry event payloads are published to the workspace.
-- ---------------------------------------------------------------------------------------------
ALTER TABLE agent_task_status ADD COLUMN IF NOT EXISTS lease_until timestamptz;
ALTER TABLE agent_task_status ADD COLUMN IF NOT EXISTS lease_seconds integer NOT NULL DEFAULT 600
    CHECK (lease_seconds BETWEEN 60 AND 3600);
ALTER TABLE agent_task_status ADD COLUMN IF NOT EXISTS attempts integer NOT NULL DEFAULT 1
    CHECK (attempts BETWEEN 1 AND 3);
ALTER TABLE agent_task_status ADD COLUMN IF NOT EXISTS retry_notice_pending boolean NOT NULL DEFAULT false;
CREATE INDEX IF NOT EXISTS agent_task_status_lease_idx
    ON agent_task_status (lease_until) WHERE phase = 'Running';
CREATE INDEX IF NOT EXISTS agent_task_status_retry_notice_idx
    ON agent_task_status (task_id) WHERE retry_notice_pending;

-- Adopt existing in-flight tasks once, including rows with no recorded start time. Reapplying
-- the schema must never extend an active claim or reset its attempt counter.
UPDATE agent_task_status SET lease_until = now() + interval '10 minutes'
    WHERE phase = 'Running' AND lease_until IS NULL;

CREATE TABLE IF NOT EXISTS agent_task_leases_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0042_agent_task_leases')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0043_agent_launch_workspaces: agent launches and cron schedules belong to one workspace
-- (lake#442). The operator places a launch's Job, Deployment or CronJob in the namespace of the
-- workspace that holds the agent and reads `workspace_id` to find it; without the column it could
-- read no launch at all, so every launch stayed at phase ''. Lake lists a workspace's own runs
-- and schedules only. `stop_reason` records why lake itself withdrew a launch: one the operator
-- does not pick up within five minutes is withdrawn with a reason the person can read.
-- ---------------------------------------------------------------------------------------------
ALTER TABLE agent_launches ADD COLUMN IF NOT EXISTS workspace_id uuid REFERENCES workspaces(id);
ALTER TABLE agent_launches ADD COLUMN IF NOT EXISTS stop_reason text NOT NULL DEFAULT '';
ALTER TABLE agent_cron_schedules ADD COLUMN IF NOT EXISTS workspace_id uuid
    REFERENCES workspaces(id);
CREATE INDEX IF NOT EXISTS agent_launches_workspace_idx
    ON agent_launches (workspace_id, created_at DESC);
CREATE INDEX IF NOT EXISTS agent_cron_schedules_workspace_idx
    ON agent_cron_schedules (workspace_id, created_at);

DO $$
DECLARE
    target_table text;
    unresolved record;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM schema_migrations
                   WHERE version = '0043_agent_launch_workspaces') THEN
        -- Earlier rows name no workspace. Assign them only where the space has exactly one live
        -- workspace, and record every outcome, as 0038 did. The routing triggers below would
        -- refuse the assignment as a change of workspace; they are created again afterwards.
        DROP TRIGGER IF EXISTS agent_launch_workspace_routing ON agent_launches;
        DROP TRIGGER IF EXISTS agent_cron_schedule_workspace_routing ON agent_cron_schedules;
        FOREACH target_table IN ARRAY ARRAY['agent_launches', 'agent_cron_schedules'] LOOP
            EXECUTE format(
                'INSERT INTO workspace_assignments (object_table, object_id, customer_id, workspace_id, outcome)
                 SELECT %L, o.id, o.customer_id, w.id,
                        CASE WHEN w.id IS NULL THEN ''needs_selection'' ELSE ''sole_workspace'' END
                 FROM %I o LEFT JOIN workspaces w ON w.customer_id = o.customer_id
                      AND w.desired <> ''deleted''
                      AND (SELECT count(*) FROM workspaces w2 WHERE w2.customer_id = o.customer_id
                           AND w2.desired <> ''deleted'') = 1
                 WHERE o.workspace_id IS NULL ON CONFLICT DO NOTHING', target_table, target_table);
            EXECUTE format(
                'UPDATE %I o SET workspace_id = a.workspace_id FROM workspace_assignments a
                 WHERE a.object_table = %L AND a.object_id = o.id
                       AND o.workspace_id IS NULL AND a.workspace_id IS NOT NULL',
                target_table, target_table);
        END LOOP;
        -- A launch the operator never saw was blocked by the missing column. Running it now, long
        -- after it was asked for, would surprise the person who asked: withdraw it with a reason.
        UPDATE agent_launches l SET desired = 'absent', generation = generation + 1,
               stop_reason = 'Timed out: the Orama operator did not start this run within '
                             'five minutes.',
               updated_at = now()
         WHERE l.desired = 'present' AND l.created_at < now() - interval '5 minutes'
           AND NOT EXISTS (SELECT 1 FROM agent_launch_status ls WHERE ls.launch_id = l.id);
        FOR unresolved IN SELECT * FROM workspace_assignments
                           WHERE workspace_id IS NULL
                             AND object_table IN ('agent_launches', 'agent_cron_schedules') LOOP
            RAISE WARNING 'workspace_id requires selection: table=%, object=%, customer=%',
                unresolved.object_table, unresolved.object_id, unresolved.customer_id;
        END LOOP;
    END IF;
END $$;

-- New rows must name a live workspace of their own space; neither column changes afterwards.
DROP TRIGGER IF EXISTS agent_launch_workspace_routing ON agent_launches;
CREATE TRIGGER agent_launch_workspace_routing BEFORE INSERT OR UPDATE ON agent_launches
    FOR EACH ROW EXECUTE FUNCTION enforce_workspace_routing();
DROP TRIGGER IF EXISTS agent_cron_schedule_workspace_routing ON agent_cron_schedules;
CREATE TRIGGER agent_cron_schedule_workspace_routing BEFORE INSERT OR UPDATE ON agent_cron_schedules
    FOR EACH ROW EXECUTE FUNCTION enforce_workspace_routing();

CREATE TABLE IF NOT EXISTS agent_launch_workspaces_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0043_agent_launch_workspaces')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0044_store_workspaces: a store belongs to one workspace (lake#465). The operator places a
-- store's claim in the namespace of its workspace and reads `stores.workspace_id` to find it;
-- without the column it could read no store at all, so no store was created or deleted. Lake
-- lists, hands out and deletes a workspace's own stores only. Names stay unique per space, as
-- for services and apps.
-- ---------------------------------------------------------------------------------------------
ALTER TABLE stores ADD COLUMN IF NOT EXISTS workspace_id uuid REFERENCES workspaces(id);
CREATE INDEX IF NOT EXISTS stores_workspace_idx ON stores (workspace_id, created_at);

DO $$
DECLARE
    unresolved record;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '0044_store_workspaces') THEN
        -- Earlier stores name no workspace. Assign them only where the space has exactly one live
        -- workspace, and record every outcome, as 0043 did. The routing trigger below would
        -- refuse the assignment as a change of workspace; it is created again afterwards.
        DROP TRIGGER IF EXISTS store_workspace_routing ON stores;
        INSERT INTO workspace_assignments (object_table, object_id, customer_id, workspace_id, outcome)
            SELECT 'stores', o.id, o.customer_id, w.id,
                   CASE WHEN w.id IS NULL THEN 'needs_selection' ELSE 'sole_workspace' END
            FROM stores o LEFT JOIN workspaces w ON w.customer_id = o.customer_id
                 AND w.desired <> 'deleted'
                 AND (SELECT count(*) FROM workspaces w2 WHERE w2.customer_id = o.customer_id
                      AND w2.desired <> 'deleted') = 1
            WHERE o.workspace_id IS NULL ON CONFLICT DO NOTHING;
        UPDATE stores o SET workspace_id = a.workspace_id FROM workspace_assignments a
            WHERE a.object_table = 'stores' AND a.object_id = o.id
              AND o.workspace_id IS NULL AND a.workspace_id IS NOT NULL;
        FOR unresolved IN SELECT * FROM workspace_assignments
                           WHERE workspace_id IS NULL AND object_table = 'stores' LOOP
            RAISE WARNING 'workspace_id requires selection: table=%, object=%, customer=%',
                unresolved.object_table, unresolved.object_id, unresolved.customer_id;
        END LOOP;
    END IF;
END $$;

-- New stores must name a live workspace of their own space; neither column changes afterwards.
DROP TRIGGER IF EXISTS store_workspace_routing ON stores;
CREATE TRIGGER store_workspace_routing BEFORE INSERT OR UPDATE ON stores
    FOR EACH ROW EXECUTE FUNCTION enforce_workspace_routing();

CREATE TABLE IF NOT EXISTS store_workspaces_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0044_store_workspaces')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0045_ask_message_workspaces: Ask Orama conversations live in the workspace Zetta that owns them
-- (lake#370, docs/architecture/workspace-records.md). Lake no longer writes or reads
-- ask_messages; the operator's one-shot copy (copy_workspace_records) moves each row into its
-- workspace and verifies it. This migration only records each row's owner: a space with exactly
-- one live workspace owns its rows, every other row is recorded as needs_selection, and a row with
-- no space is reported. No column is added and nothing is dropped; a later migration drops the
-- table once the copy has been verified on every environment.
-- ---------------------------------------------------------------------------------------------
DO $$
DECLARE
    unresolved record;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '0045_ask_message_workspaces') THEN
        INSERT INTO workspace_assignments (object_table, object_id, customer_id, workspace_id, outcome)
            SELECT 'ask_messages', m.id, m.customer_id, w.id,
                   CASE WHEN w.id IS NULL THEN 'needs_selection' ELSE 'sole_workspace' END
            FROM ask_messages m LEFT JOIN workspaces w ON w.customer_id = m.customer_id
                 AND w.desired <> 'deleted'
                 AND (SELECT count(*) FROM workspaces w2 WHERE w2.customer_id = m.customer_id
                      AND w2.desired <> 'deleted') = 1
            WHERE m.customer_id IS NOT NULL ON CONFLICT DO NOTHING;
        FOR unresolved IN SELECT * FROM workspace_assignments
                           WHERE workspace_id IS NULL AND object_table = 'ask_messages' LOOP
            RAISE WARNING 'workspace_id requires selection: table=%, object=%, customer=%',
                unresolved.object_table, unresolved.object_id, unresolved.customer_id;
        END LOOP;
        FOR unresolved IN SELECT id FROM ask_messages WHERE customer_id IS NULL LOOP
            RAISE WARNING 'ask message has no space and is not copied: object=%', unresolved.id;
        END LOOP;
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS ask_message_workspaces_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0045_ask_message_workspaces')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0046_opportunity_workspaces: Design → Opportunities live in the workspace Zetta of the workspace
-- their scan ran in (lake#376, docs/architecture/workspace-records.md). Lake no longer writes or
-- reads opportunities; the operator's one-shot copy (copy_workspace_records) moves each row into
-- its workspace and verifies it. This migration only records each row's owner: the workspace the
-- scan's agent task names owns its rows (task_workspace); a task that names none falls back to a
-- space with exactly one live workspace (sole_workspace); a task whose workspace is deleted is
-- recorded as deleted_workspace and every other row as needs_selection, both without a workspace.
-- No column is added and nothing is dropped; a later migration drops the table once the copy has
-- been verified on every environment.
-- ---------------------------------------------------------------------------------------------
DO $$
DECLARE
    unresolved record;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '0046_opportunity_workspaces') THEN
        INSERT INTO workspace_assignments (object_table, object_id, customer_id, workspace_id, outcome)
            SELECT 'opportunities', o.id, o.customer_id,
                   CASE WHEN t.workspace_id IS NULL THEN w.id WHEN tw.id IS NOT NULL THEN tw.id END,
                   CASE WHEN tw.id IS NOT NULL THEN 'task_workspace'
                        WHEN t.workspace_id IS NOT NULL THEN 'deleted_workspace'
                        WHEN w.id IS NOT NULL THEN 'sole_workspace'
                        ELSE 'needs_selection' END
            FROM opportunities o JOIN agent_tasks t ON t.id = o.task_id
                 LEFT JOIN workspaces tw ON tw.id = t.workspace_id
                      AND tw.customer_id = o.customer_id AND tw.desired <> 'deleted'
                 LEFT JOIN workspaces w ON w.customer_id = o.customer_id
                      AND w.desired <> 'deleted'
                      AND (SELECT count(*) FROM workspaces w2 WHERE w2.customer_id = o.customer_id
                           AND w2.desired <> 'deleted') = 1
            ON CONFLICT DO NOTHING;
        FOR unresolved IN SELECT * FROM workspace_assignments
                           WHERE workspace_id IS NULL AND object_table = 'opportunities' LOOP
            RAISE WARNING 'workspace_id requires selection: table=%, object=%, customer=%, outcome=%',
                unresolved.object_table, unresolved.object_id, unresolved.customer_id,
                unresolved.outcome;
        END LOOP;
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS opportunity_workspaces_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0046_opportunity_workspaces')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0047_stream_owner_workspaces: topic creators live in the workspace Zetta whose topic they
-- describe (lake#379, docs/control-plane/streams.md, docs/architecture/workspace-records.md).
-- Lake no longer writes or reads stream_owners; the operator's one-shot copy
-- (copy_workspace_records) moves each row into its workspace and verifies it. Each row already
-- names its workspace, so this migration only records it: a row whose workspace is live in the
-- same space is owned by it, a row whose workspace was deleted is recorded as workspace_deleted
-- (its topic went with that incarnation, so there is nothing to copy), and any other row is
-- recorded as unknown_workspace and reported. A row's object_id is
-- md5(customer_id, workspace_id, topic) as a UUID, since the table has no id column. No column is
-- added and nothing is dropped; a later migration drops the table once the copy has been verified
-- on every environment.
-- ---------------------------------------------------------------------------------------------
DO $$
DECLARE
    unresolved record;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '0047_stream_owner_workspaces') THEN
        INSERT INTO workspace_assignments (object_table, object_id, customer_id, workspace_id, outcome)
            SELECT 'stream_owners',
                   md5(s.customer_id::text || E'\n' || s.workspace_id || E'\n' || s.topic)::uuid,
                   s.customer_id,
                   CASE WHEN w.desired <> 'deleted' THEN w.id END,
                   CASE WHEN w.id IS NULL THEN 'unknown_workspace'
                        WHEN w.desired = 'deleted' THEN 'workspace_deleted'
                        ELSE 'recorded_workspace' END
            FROM stream_owners s LEFT JOIN workspaces w ON w.id::text = s.workspace_id
                 AND w.customer_id = s.customer_id
            ON CONFLICT DO NOTHING;
        FOR unresolved IN SELECT * FROM workspace_assignments
                           WHERE workspace_id IS NULL AND object_table = 'stream_owners' LOOP
            RAISE WARNING 'stream owner is not copied (%): object=%, customer=%',
                unresolved.outcome, unresolved.object_id, unresolved.customer_id;
        END LOOP;
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS stream_owner_workspaces_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0047_stream_owner_workspaces')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0048_drive_share_link_workspaces: drive share links live in the workspace Zetta that holds them
-- (lake#350, docs/api/sharing.md, docs/architecture/workspace-records.md). A new link's token
-- names its workspace and record, so resolving it needs no central lookup. Lake no longer writes
-- or reads drive_share_links; the operator's one-shot copy (copy_workspace_records) moves each row
-- into its workspace and verifies it. This migration records each row's owner (the only live
-- workspace of the repository's space; otherwise needs_selection) and, for each assigned row, a
-- content-free route from the token digest a pre-move token presents to the workspace and record
-- key. The route holds no path, role, expiry or creator, and outlives the later migration that
-- drops drive_share_links. Nothing is dropped here.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS drive_share_link_routes (
    token_hash   text        PRIMARY KEY,
    link_id      uuid        NOT NULL UNIQUE,
    workspace_id uuid        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    recorded_at  timestamptz NOT NULL DEFAULT now()
);

DO $$
DECLARE
    unresolved record;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM schema_migrations
                   WHERE version = '0048_drive_share_link_workspaces') THEN
        INSERT INTO workspace_assignments (object_table, object_id, customer_id, workspace_id, outcome)
            SELECT 'drive_share_links', l.id, r.customer_id, w.id,
                   CASE WHEN w.id IS NULL THEN 'needs_selection' ELSE 'sole_workspace' END
            FROM drive_share_links l JOIN repositories r ON r.id = l.repository_id
                 LEFT JOIN workspaces w ON w.customer_id = r.customer_id
                 AND w.desired <> 'deleted'
                 AND (SELECT count(*) FROM workspaces w2 WHERE w2.customer_id = r.customer_id
                      AND w2.desired <> 'deleted') = 1
            ON CONFLICT DO NOTHING;
        INSERT INTO drive_share_link_routes (token_hash, link_id, workspace_id)
            SELECT l.token_hash, l.id, a.workspace_id
            FROM drive_share_links l JOIN workspace_assignments a
                 ON a.object_table = 'drive_share_links' AND a.object_id = l.id
            WHERE a.workspace_id IS NOT NULL
            ON CONFLICT (token_hash) DO UPDATE SET workspace_id = EXCLUDED.workspace_id;
        FOR unresolved IN SELECT * FROM workspace_assignments
                           WHERE workspace_id IS NULL AND object_table = 'drive_share_links' LOOP
            RAISE WARNING 'workspace_id requires selection: table=%, object=%, customer=%',
                unresolved.object_table, unresolved.object_id, unresolved.customer_id;
        END LOOP;
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS drive_share_link_workspaces_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0048_drive_share_link_workspaces')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0049_application_webhook_workspaces: an application's lifecycle events and their delivery
-- attempts live in the workspace Zetta of the application's workspace (lake#356, lake#357,
-- docs/architecture/workspace-records.md). Lake no longer writes or reads
-- auth_app_webhook_events or auth_app_webhook_deliveries; the operator's one-shot copy
-- (copy_workspace_records) moves each row into its workspace and verifies it.
--
-- auth_applications gains workspace_id, the routing pointer that names which workspace an
-- application's events belong to: content-free multitenancy, like stores.workspace_id (0044).
-- register_application requires it; it is set once and never changes. An application whose space
-- has exactly one live workspace is assigned it here; every other application is recorded as
-- needs_selection and may be assigned once (NULL to a workspace of its space) after review.
--
-- Each event row's owner is recorded in workspace_assignments: its application's workspace, or
-- needs_selection. A delivery attempt has no row of its own there; it always goes where its event
-- goes. No table is dropped; a later migration drops both once the copy has been verified on
-- every environment.
-- ---------------------------------------------------------------------------------------------
ALTER TABLE auth_applications ADD COLUMN IF NOT EXISTS workspace_id uuid REFERENCES workspaces(id);
CREATE INDEX IF NOT EXISTS auth_applications_workspace_idx ON auth_applications (workspace_id);

-- New applications name a live workspace of their own space; a legacy application without one may
-- be assigned once. Neither the workspace nor the space changes afterwards.
CREATE OR REPLACE FUNCTION enforce_application_workspace() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF NEW.customer_id IS DISTINCT FROM OLD.customer_id OR
           (OLD.workspace_id IS NOT NULL AND NEW.workspace_id IS DISTINCT FROM OLD.workspace_id) THEN
            RAISE EXCEPTION 'workspace_id and customer_id are immutable' USING ERRCODE = '23514';
        END IF;
        -- Any other change keeps the assignment as it was, including none yet.
        IF NEW.workspace_id IS NOT DISTINCT FROM OLD.workspace_id THEN
            RETURN NEW;
        END IF;
    END IF;
    IF NEW.workspace_id IS NULL OR NOT EXISTS (
        SELECT 1 FROM workspaces WHERE id = NEW.workspace_id AND customer_id = NEW.customer_id
            AND desired <> 'deleted'
    ) THEN
        RAISE EXCEPTION 'workspace_id must name a workspace in this space' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

DO $$
DECLARE
    unresolved record;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM schema_migrations
                   WHERE version = '0049_application_webhook_workspaces') THEN
        DROP TRIGGER IF EXISTS application_workspace_routing ON auth_applications;
        INSERT INTO workspace_assignments (object_table, object_id, customer_id, workspace_id, outcome)
            SELECT 'auth_applications', a.id, a.customer_id, w.id,
                   CASE WHEN w.id IS NULL THEN 'needs_selection' ELSE 'sole_workspace' END
            FROM auth_applications a LEFT JOIN workspaces w ON w.customer_id = a.customer_id
                 AND w.desired <> 'deleted'
                 AND (SELECT count(*) FROM workspaces w2 WHERE w2.customer_id = a.customer_id
                      AND w2.desired <> 'deleted') = 1
            WHERE a.workspace_id IS NULL ON CONFLICT DO NOTHING;
        UPDATE auth_applications a SET workspace_id = s.workspace_id FROM workspace_assignments s
            WHERE s.object_table = 'auth_applications' AND s.object_id = a.id
              AND a.workspace_id IS NULL AND s.workspace_id IS NOT NULL;
        INSERT INTO workspace_assignments (object_table, object_id, customer_id, workspace_id, outcome)
            SELECT 'auth_app_webhook_events', e.id, a.customer_id, a.workspace_id,
                   CASE WHEN a.workspace_id IS NULL THEN 'needs_selection'
                        ELSE 'application_workspace' END
            FROM auth_app_webhook_events e JOIN auth_applications a ON a.id = e.application_id
            ON CONFLICT DO NOTHING;
        FOR unresolved IN SELECT * FROM workspace_assignments WHERE workspace_id IS NULL
                           AND object_table IN ('auth_applications', 'auth_app_webhook_events') LOOP
            RAISE WARNING 'workspace_id requires selection: table=%, object=%, customer=%',
                unresolved.object_table, unresolved.object_id, unresolved.customer_id;
        END LOOP;
    END IF;
END $$;

DROP TRIGGER IF EXISTS application_workspace_routing ON auth_applications;
CREATE TRIGGER application_workspace_routing BEFORE INSERT OR UPDATE ON auth_applications
    FOR EACH ROW EXECUTE FUNCTION enforce_application_workspace();

CREATE TABLE IF NOT EXISTS application_webhook_workspaces_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0049_application_webhook_workspaces')
    ON CONFLICT DO NOTHING;
