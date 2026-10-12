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
    -- description was a column here; 0070_drop_moved_tables dropped it (lake#315 part 2).
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
-- Lifecycle operations are recorded by the signed audit stream above through the capability
-- registry.
-- ---------------------------------------------------------------------------------------------
ALTER TABLE repositories ADD COLUMN IF NOT EXISTS deleted_at timestamptz;
ALTER TABLE repositories ADD COLUMN IF NOT EXISTS retention_until timestamptz;
ALTER TABLE repositories DROP CONSTRAINT IF EXISTS repository_visibility_check;
ALTER TABLE repositories ADD CONSTRAINT repository_visibility_check
    CHECK (visibility IN ('private', 'organization', 'public'));

CREATE INDEX IF NOT EXISTS repositories_live_customer_id_idx
    ON repositories (customer_id) WHERE deleted_at IS NULL;

-- 0006 also created repository_refs, a copy of each liquid ref that nothing read. 0064 drops it
-- (lake#319), so it is no longer created: a rerun of this file must not bring it back.

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

-- The mounts, entries, versions, named encryption keys and data keys were the secrets_mounts,
-- secrets_entries, secrets_versions, secrets_encryption_keys and secrets_data_keys tables here.
-- They live in workspace Zettas (0057, lake#328-#332) and 0070_drop_moved_tables drops the
-- tables, so they are no longer created: a rerun of this file must not bring them back.

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
-- Forge records were the forge_records table here. They live in workspace Zettas (0054,
-- lake#341) and 0070_drop_moved_tables drops the table, so it is no longer created.

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
-- Usage was the metering_usage and metering_build_records tables here. It lives in workspace
-- Zettas (0053, lake#348, lake#349) and 0070_drop_moved_tables drops the tables, so they are no
-- longer created.

CREATE TABLE IF NOT EXISTS metering_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES ('0014_metering') ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- Drive sharing (migration 0015): expiring, scoped link capabilities over path policy objects.
-- Direct recipient shares remain ordinary grants on the same objects.
-- ---------------------------------------------------------------------------------------------
-- Share links were the drive_share_links table here. They live in workspace Zettas (0048,
-- lake#350) and 0070_drop_moved_tables drops the table, so it is no longer created;
-- drive_share_link_routes (0048) stays.

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
    -- name, allowed_origins, redirect_uris and theme were columns here; they live in the
    -- application's workspace Zetta (0059, lake#351) and 0070_drop_moved_tables dropped them.
    custom_sign_in_domain text,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS auth_applications_custom_domain_unique
    ON auth_applications (lower(custom_sign_in_domain))
    WHERE custom_sign_in_domain IS NOT NULL;
CREATE INDEX IF NOT EXISTS auth_applications_customer_idx
    ON auth_applications (customer_id, created_at DESC);
DROP TRIGGER IF EXISTS auth_applications_set_updated_at ON auth_applications;
CREATE TRIGGER auth_applications_set_updated_at BEFORE UPDATE ON auth_applications
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Signing keys, users, organizations, webhooks, events and deliveries were the
-- auth_app_signing_keys, auth_app_users, auth_app_organizations, auth_app_webhooks,
-- auth_app_webhook_events and auth_app_webhook_deliveries tables here. They live in the
-- application's workspace Zetta (0049, 0055, 0056; lake#352-#357) and 0070_drop_moved_tables
-- drops the tables, so they are no longer created.

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
-- Environments and the mirror were the platform_environments and platform_mirror tables here.
-- They live in workspace Zettas (0060, lake#358, lake#359) and 0070_drop_moved_tables drops the
-- tables, so they are no longer created; each environment's policy object stays.

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

-- One-use OIDC authorization codes were the auth_app_authorization_codes table here. They live
-- in the application's workspace Zetta (0061, lake#363) and 0070_drop_moved_tables drops the
-- table, so it is no longer created.

ALTER TABLE pki_certificates ADD COLUMN IF NOT EXISTS revoked_at timestamptz;
ALTER TABLE pki_certificates DROP CONSTRAINT IF EXISTS pki_certificates_profile_check;
ALTER TABLE pki_certificates ADD CONSTRAINT pki_certificates_profile_check
    CHECK (profile IN ('x509_svid', 'jwt_svid', 'acme', 'ssh', 'client', 'device', 'agent'));
ALTER TABLE sessions DROP CONSTRAINT IF EXISTS sessions_kind_check;
ALTER TABLE sessions ADD CONSTRAINT sessions_kind_check
    CHECK (kind IN ('browser', 'api_token', 'static', 'admin', 'taap'));

-- Dynamic leases were the secrets_dynamic_leases table here. They live in workspace Zettas (0058,
-- lake#362) and 0070_drop_moved_tables drops the table, so it is no longer created;
-- secret_lease_routes (0058) stays.

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

-- Ask Orama conversations were the ask_messages table here. They live in workspace Zettas
-- (0045, lake#370) and 0050_drop_ask_messages drops the table, so it is no longer created: a
-- rerun of this file must not bring it back.

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
    -- The request (body) the agent receives is the task's record in its workspace Zetta (0062,
    -- lake#374); 0070_drop_moved_tables dropped the retired body column.
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
    -- message and result moved into the task's record (0062, lake#375); 0070 dropped them.
    credential_sha256   text        NOT NULL DEFAULT '',
    started_at          timestamptz,
    finished_at         timestamptz,
    observed_generation bigint      NOT NULL DEFAULT 0,
    updated_at          timestamptz NOT NULL DEFAULT now()
);

-- Design → Opportunities (lake#269): what Auto-Suggest recommended, kept across scans. A scan task
-- that finished writes its rows once; a person turns them into projects or dismisses them.
-- Design → Opportunities were the opportunities table here. They live in workspace Zettas (0046,
-- lake#376) and 0070_drop_moved_tables drops the table, so it is no longer created.

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
-- Topic creators were the stream_owners table here. They live in workspace Zettas (0047,
-- lake#379) and 0070_drop_moved_tables drops the table, so it is no longer created.
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
    -- The table exists only on a database that had it before 0045; 0050 drops it afterwards.
    IF NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '0045_ask_message_workspaces')
       AND to_regclass('ask_messages') IS NOT NULL THEN
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
    IF NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '0046_opportunity_workspaces')
       AND to_regclass('opportunities') IS NOT NULL THEN
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
    IF NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '0047_stream_owner_workspaces')
       AND to_regclass('stream_owners') IS NOT NULL THEN
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
                   WHERE version = '0048_drive_share_link_workspaces')
       AND to_regclass('drive_share_links') IS NOT NULL THEN
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
        -- 0070_drop_moved_tables drops the events table; a fresh database never has it.
        IF to_regclass('auth_app_webhook_events') IS NOT NULL THEN
            INSERT INTO workspace_assignments (object_table, object_id, customer_id, workspace_id,
                                               outcome)
                SELECT 'auth_app_webhook_events', e.id, a.customer_id, a.workspace_id,
                       CASE WHEN a.workspace_id IS NULL THEN 'needs_selection'
                            ELSE 'application_workspace' END
                FROM auth_app_webhook_events e JOIN auth_applications a ON a.id = e.application_id
                ON CONFLICT DO NOTHING;
        END IF;
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

-- ---------------------------------------------------------------------------------------------
-- 0050_drop_ask_messages: the retired ask_messages table is dropped (lake#483). Ask Orama
-- conversations live in each workspace Zetta's lake_ask_messages collection (0045, lake#370); the
-- operator's copy into them was verified on dev (15 of 15 rows, complete) before this migration.
-- The workspace_assignments rows 0045 recorded for the table stay: they are the record of where
-- each row went. The earlier CREATE TABLE above is gone and 0045's block runs only while the
-- table exists, so a rerun of this file leaves it dropped.
-- ---------------------------------------------------------------------------------------------
DROP TABLE IF EXISTS ask_messages;

CREATE TABLE IF NOT EXISTS drop_ask_messages_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0050_drop_ask_messages')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0051_repository_workspaces: a repository belongs to one workspace (lake#315, part 1: routing).
-- repositories.workspace_id is a content-free routing pointer, like stores.workspace_id (0044):
-- it names the workspace whose Zetta will hold the repository's metadata and content when they
-- move (lake#315 part 2, lake#341 for forge records, lake#380 for the store). create_repository,
-- import_repository and import_github_repository require it. Nothing moves here.
--
-- A repository of a space with exactly one live workspace is assigned it; every other repository
-- is recorded as needs_selection and reported. New repositories go through
-- enforce_workspace_routing (trigger repository_workspace_routing): the workspace must be a live
-- workspace of the repository's own space, and neither the workspace nor the space changes
-- afterwards, so a repository is no longer transferred between spaces. As for 0038, an unresolved
-- row is assigned only by an explicitly reviewed migration.
-- ---------------------------------------------------------------------------------------------
ALTER TABLE repositories ADD COLUMN IF NOT EXISTS workspace_id uuid REFERENCES workspaces(id);
CREATE INDEX IF NOT EXISTS repositories_workspace_idx ON repositories (workspace_id, created_at);

DO $$
DECLARE
    unresolved record;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '0051_repository_workspaces') THEN
        -- Earlier repositories name no workspace. Assign them only where the space has exactly one
        -- live workspace, and record every outcome, tombstoned repositories included, as 0044
        -- did. The routing trigger below would refuse the assignment as a change of workspace; it
        -- is created again afterwards.
        DROP TRIGGER IF EXISTS repository_workspace_routing ON repositories;
        INSERT INTO workspace_assignments (object_table, object_id, customer_id, workspace_id, outcome)
            SELECT 'repositories', o.id, o.customer_id, w.id,
                   CASE WHEN w.id IS NULL THEN 'needs_selection' ELSE 'sole_workspace' END
            FROM repositories o LEFT JOIN workspaces w ON w.customer_id = o.customer_id
                 AND w.desired <> 'deleted'
                 AND (SELECT count(*) FROM workspaces w2 WHERE w2.customer_id = o.customer_id
                      AND w2.desired <> 'deleted') = 1
            WHERE o.workspace_id IS NULL ON CONFLICT DO NOTHING;
        UPDATE repositories o SET workspace_id = a.workspace_id FROM workspace_assignments a
            WHERE a.object_table = 'repositories' AND a.object_id = o.id
              AND o.workspace_id IS NULL AND a.workspace_id IS NOT NULL;
        FOR unresolved IN SELECT * FROM workspace_assignments
                           WHERE workspace_id IS NULL AND object_table = 'repositories' LOOP
            RAISE WARNING 'workspace_id requires selection: table=%, object=%, customer=%',
                unresolved.object_table, unresolved.object_id, unresolved.customer_id;
        END LOOP;
    END IF;
END $$;

-- New repositories must name a live workspace of their own space; neither column changes
-- afterwards.
DROP TRIGGER IF EXISTS repository_workspace_routing ON repositories;
CREATE TRIGGER repository_workspace_routing BEFORE INSERT OR UPDATE ON repositories
    FOR EACH ROW EXECUTE FUNCTION enforce_workspace_routing();

CREATE TABLE IF NOT EXISTS repository_workspaces_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0051_repository_workspaces')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0053_metering_workspaces: repository usage lives in the workspace Zetta of the repository's
-- workspace (lake#348, lake#349, docs/api/metering.md, docs/architecture/workspace-records.md).
-- Lake no longer writes or reads metering_usage or metering_build_records: transfer is counted
-- in memory and added to each workspace's hourly record off the request path, every build
-- result is one record in its repository's workspace, and repository storage is measured live on
-- each read and not recorded per hour (lake#380 makes stores per workspace). The operator's
-- one-shot copy (copy_workspace_records) moves each row's transfer and each build record into
-- its workspace and verifies it.
--
-- This migration only records each row's owner: a space with exactly one live workspace owns its
-- rows; every other row is recorded as needs_selection and reported. A usage row's id is
-- md5(customer_id, hour as YYYY-MM-DDTHH:MI:SSZ) and a build row's md5(customer_id, result_id),
-- joined by a newline, as a UUID, since neither table has an id column. Usage rows without
-- transfer carry only the retired storage gauge and build compute (which the build records hold),
-- so they are not assigned or copied. No column is added and nothing is dropped; a later
-- migration drops both tables once the copy has been verified on every environment.
-- ---------------------------------------------------------------------------------------------
DO $$
DECLARE
    unresolved record;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '0053_metering_workspaces')
       AND to_regclass('metering_usage') IS NOT NULL THEN
        INSERT INTO workspace_assignments (object_table, object_id, customer_id, workspace_id, outcome)
            SELECT 'metering_usage',
                   md5(m.customer_id::text || E'\n' ||
                       to_char(m.hour AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'))::uuid,
                   m.customer_id, w.id,
                   CASE WHEN w.id IS NULL THEN 'needs_selection' ELSE 'sole_workspace' END
            FROM metering_usage m LEFT JOIN workspaces w ON w.customer_id = m.customer_id
                 AND w.desired <> 'deleted'
                 AND (SELECT count(*) FROM workspaces w2 WHERE w2.customer_id = m.customer_id
                      AND w2.desired <> 'deleted') = 1
            WHERE m.transfer_in_bytes > 0 OR m.transfer_out_bytes > 0
            ON CONFLICT DO NOTHING;
        INSERT INTO workspace_assignments (object_table, object_id, customer_id, workspace_id, outcome)
            SELECT 'metering_build_records', md5(b.customer_id::text || E'\n' || b.result_id)::uuid,
                   b.customer_id, w.id,
                   CASE WHEN w.id IS NULL THEN 'needs_selection' ELSE 'sole_workspace' END
            FROM metering_build_records b LEFT JOIN workspaces w ON w.customer_id = b.customer_id
                 AND w.desired <> 'deleted'
                 AND (SELECT count(*) FROM workspaces w2 WHERE w2.customer_id = b.customer_id
                      AND w2.desired <> 'deleted') = 1
            ON CONFLICT DO NOTHING;
        FOR unresolved IN SELECT * FROM workspace_assignments WHERE workspace_id IS NULL
                           AND object_table IN ('metering_usage', 'metering_build_records') LOOP
            RAISE WARNING 'workspace_id requires selection: table=%, object=%, customer=%',
                unresolved.object_table, unresolved.object_id, unresolved.customer_id;
        END LOOP;
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS metering_workspaces_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0053_metering_workspaces')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0054_forge_record_workspaces: forge records (issues, issue comments, reviews, review comments,
-- releases, webhooks, webhook deliveries, branch protection rules, exports and GitHub import
-- reports) live in the workspace Zetta of their repository's workspace (lake#341,
-- docs/api/forge.md, docs/architecture/workspace-records.md). Lake no longer writes or reads
-- forge_records; the operator's one-shot copy (copy_workspace_records) moves each row into its
-- workspace and verifies it.
--
-- Each row's owner is its repository's workspace (repositories.workspace_id, 0051), recorded in
-- workspace_assignments as repository_workspace. A row whose repository has no workspace yet is
-- needs_selection and is reported; the copy assigns it once the repository is assigned. A row
-- whose repository's workspace was deleted is workspace_deleted: it went with that workspace and
-- does not hold the copy back. Nothing is dropped here; a later migration drops forge_records once
-- the copy has been verified on every environment.
-- ---------------------------------------------------------------------------------------------
DO $$
DECLARE
    unresolved record;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '0054_forge_record_workspaces')
       AND to_regclass('forge_records') IS NOT NULL THEN
        INSERT INTO workspace_assignments (object_table, object_id, customer_id, workspace_id, outcome)
            SELECT 'forge_records', f.id, r.customer_id,
                   CASE WHEN w.desired = 'deleted' THEN NULL ELSE r.workspace_id END,
                   CASE WHEN r.workspace_id IS NULL THEN 'needs_selection'
                        WHEN w.desired = 'deleted' THEN 'workspace_deleted'
                        ELSE 'repository_workspace' END
            FROM forge_records f JOIN repositories r ON r.id = f.repository_id
                 LEFT JOIN workspaces w ON w.id = r.workspace_id
            ON CONFLICT DO NOTHING;
        FOR unresolved IN SELECT * FROM workspace_assignments
                           WHERE workspace_id IS NULL AND object_table = 'forge_records' LOOP
            RAISE WARNING 'forge record not copied (%): object=%, customer=%',
                unresolved.outcome, unresolved.object_id, unresolved.customer_id;
        END LOOP;
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS forge_record_workspaces_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0054_forge_record_workspaces')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0055_application_user_workspaces: an application's users (lake#353) and organizations
-- (lake#354) live in the workspace Zetta of the application's workspace
-- (auth_applications.workspace_id, 0049; docs/api/apps.md, docs/architecture/
-- workspace-records.md). Lake no longer writes or reads auth_app_users or auth_app_organizations;
-- the operator's one-shot copy (copy_workspace_records) moves each row into its workspace, with
-- the claims that keep a pool user's email, and an organization's TAAP identity and name, unique
-- within the application, and verifies it.
--
-- Each row's owner is recorded in workspace_assignments: its application's workspace
-- (application_workspace), needs_selection while the application has none, or workspace_deleted
-- when that workspace was deleted (the rows went with it and do not hold the copy back). A user
-- row's id is md5(application id, newline, user id) as a UUID, since auth_app_users has no id
-- column. Nothing is dropped here; a later migration drops both tables once the copy has been
-- verified on every environment. The users rows and the pool's credentials and sessions stay:
-- they are lake's own accounts (lake#320, lake#322, lake#323 move those).
-- ---------------------------------------------------------------------------------------------
DO $$
DECLARE
    unresolved record;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM schema_migrations
                   WHERE version = '0055_application_user_workspaces')
       AND to_regclass('auth_app_users') IS NOT NULL THEN
        INSERT INTO workspace_assignments (object_table, object_id, customer_id, workspace_id, outcome)
            SELECT r.object_table, r.object_id, a.customer_id,
                   CASE WHEN w.desired = 'deleted' THEN NULL ELSE a.workspace_id END,
                   CASE WHEN a.workspace_id IS NULL THEN 'needs_selection'
                        WHEN w.desired = 'deleted' THEN 'workspace_deleted'
                        ELSE 'application_workspace' END
            FROM (SELECT 'auth_app_users' AS object_table,
                         md5(u.application_id::text || E'\n' || u.user_id::text)::uuid AS object_id,
                         u.application_id
                  FROM auth_app_users u
                  UNION ALL
                  SELECT 'auth_app_organizations', o.id, o.application_id
                  FROM auth_app_organizations o) r
                 JOIN auth_applications a ON a.id = r.application_id
                 LEFT JOIN workspaces w ON w.id = a.workspace_id
            ON CONFLICT DO NOTHING;
        FOR unresolved IN SELECT * FROM workspace_assignments WHERE workspace_id IS NULL
                           AND object_table IN ('auth_app_users', 'auth_app_organizations') LOOP
            RAISE WARNING 'application % not copied (%): object=%, customer=%',
                unresolved.object_table, unresolved.outcome, unresolved.object_id,
                unresolved.customer_id;
        END LOOP;
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS application_user_workspaces_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0055_application_user_workspaces')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0056_application_credential_workspaces: an application's signing keys (lake#352) and its
-- lifecycle webhooks with their signing secrets (lake#355) live in the workspace Zetta of the
-- application's workspace (auth_applications.workspace_id, 0049; docs/api/apps.md,
-- docs/architecture/workspace-records.md). Lake no longer writes or reads auth_app_signing_keys
-- or auth_app_webhooks; the operator's one-shot copy (copy_workspace_records) moves each
-- application's keys (one key ring record per application) and each webhook into that workspace
-- and verifies it. The private keys and secrets move as the envelopes these tables hold and stay
-- inside the sealed records; the copy never opens them.
--
-- Each key ring's owner (object_id = the application) and each webhook's owner is recorded in
-- workspace_assignments: the application's workspace (application_workspace), needs_selection
-- while the application has none (it is reported, and resolved by the copy once the application
-- is assigned), or workspace_deleted when that workspace is gone (its records went with it and do
-- not hold the copy back). Nothing is dropped here; a later migration drops both tables once the
-- copy has been verified on every environment.
-- ---------------------------------------------------------------------------------------------
DO $$
DECLARE
    unresolved record;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM schema_migrations
                   WHERE version = '0056_application_credential_workspaces')
       AND to_regclass('auth_app_signing_keys') IS NOT NULL THEN
        INSERT INTO workspace_assignments (object_table, object_id, customer_id, workspace_id, outcome)
            SELECT c.object_table, c.object_id, a.customer_id,
                   CASE WHEN w.desired = 'deleted' THEN NULL ELSE a.workspace_id END,
                   CASE WHEN a.workspace_id IS NULL THEN 'needs_selection'
                        WHEN w.desired = 'deleted' THEN 'workspace_deleted'
                        ELSE 'application_workspace' END
            FROM (SELECT DISTINCT 'auth_app_signing_keys' AS object_table,
                         application_id AS object_id, application_id
                  FROM auth_app_signing_keys
                  UNION ALL
                  SELECT 'auth_app_webhooks', id, application_id FROM auth_app_webhooks) c
                 JOIN auth_applications a ON a.id = c.application_id
                 LEFT JOIN workspaces w ON w.id = a.workspace_id
            ON CONFLICT DO NOTHING;
        FOR unresolved IN SELECT * FROM workspace_assignments WHERE workspace_id IS NULL
                           AND object_table IN ('auth_app_signing_keys', 'auth_app_webhooks') LOOP
            RAISE WARNING 'application credential not copied (%): table=%, object=%, customer=%',
                unresolved.outcome, unresolved.object_table, unresolved.object_id,
                unresolved.customer_id;
        END LOOP;
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS application_credential_workspaces_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0056_application_credential_workspaces')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0057_secret_workspaces: secrets live in the workspace Zetta of their mount (lake#328, #329,
-- #330, #331, #332; docs/api/secrets.md, docs/architecture/workspace-records.md). A mount now
-- belongs to one workspace of its customer's space, chosen at creation (create_secret_mount takes
-- workspace_id) and never changed. The mount, its secret entries and versions, its named
-- encryption keys and every data key are sealed records of that workspace's Zetta
-- (lake_secrets_mounts, lake_secrets_entries, lake_secrets_versions,
-- lake_secrets_encryption_keys, lake_secrets_data_keys); lake no longer writes or reads the five
-- 0009 tables, which the operator's one-shot copy (copy_workspace_records) moves and verifies.
--
-- secret_mount_routes is the content-free route that remains: which workspace holds which mount,
-- with the space and organization the policy graph needs. It names no mount name, secret name,
-- value or key. The envelope is unchanged: LAKE_INSTANCE_KEK wraps the customer KEK (keystore,
-- lake#324), the customer KEK wraps each data key, and the data keys seal the secrets. keystore
-- gains the KEK a rotation replaced, kept until every workspace data key has been rewrapped.
-- secrets_dynamic_leases stays (lake#362); it loses its foreign key to secrets_mounts, since a new
-- mount has no row there.
--
-- A legacy mount is owned by the only live workspace of its space (sole_workspace) and gets its
-- route here; every other mount is needs_selection and is reported. Its entries, versions, keys
-- and data keys go where it goes and have no assignment rows of their own. Nothing is dropped; a
-- later migration drops the five tables once the copy has been verified on every environment.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS secret_mount_routes (
    mount_id        uuid        PRIMARY KEY,
    customer_id     uuid        NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    organization_id uuid        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    workspace_id    uuid        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS secret_mount_routes_workspace_idx
    ON secret_mount_routes (customer_id, workspace_id, created_at DESC);

-- A route names a live workspace of its own space; neither column changes afterwards.
DROP TRIGGER IF EXISTS secret_mount_route_workspace_routing ON secret_mount_routes;
CREATE TRIGGER secret_mount_route_workspace_routing BEFORE INSERT OR UPDATE ON secret_mount_routes
    FOR EACH ROW EXECUTE FUNCTION enforce_workspace_routing();

ALTER TABLE keystore ADD COLUMN IF NOT EXISTS previous_wrapped_key bytea;
ALTER TABLE keystore ADD COLUMN IF NOT EXISTS previous_version integer;

ALTER TABLE IF EXISTS secrets_dynamic_leases
    DROP CONSTRAINT IF EXISTS secrets_dynamic_leases_mount_id_fkey;

DO $$
DECLARE
    unresolved record;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '0057_secret_workspaces')
       AND to_regclass('secrets_mounts') IS NOT NULL THEN
        INSERT INTO workspace_assignments (object_table, object_id, customer_id, workspace_id, outcome)
            SELECT 'secrets_mounts', m.id, m.customer_id, w.id,
                   CASE WHEN w.id IS NULL THEN 'needs_selection' ELSE 'sole_workspace' END
            FROM secrets_mounts m LEFT JOIN workspaces w ON w.customer_id = m.customer_id
                 AND w.desired <> 'deleted'
                 AND (SELECT count(*) FROM workspaces w2 WHERE w2.customer_id = m.customer_id
                      AND w2.desired <> 'deleted') = 1
            ON CONFLICT DO NOTHING;
        INSERT INTO secret_mount_routes (mount_id, customer_id, organization_id, workspace_id)
            SELECT m.id, m.customer_id, m.organization_id, a.workspace_id
            FROM secrets_mounts m JOIN workspace_assignments a
                 ON a.object_table = 'secrets_mounts' AND a.object_id = m.id
                 JOIN workspaces w ON w.id = a.workspace_id AND w.customer_id = m.customer_id
                 AND w.desired <> 'deleted'
            ON CONFLICT (mount_id) DO NOTHING;
        FOR unresolved IN SELECT * FROM workspace_assignments
                           WHERE workspace_id IS NULL AND object_table = 'secrets_mounts' LOOP
            RAISE WARNING 'secret mount not copied (%): mount=%, customer=%',
                unresolved.outcome, unresolved.object_id, unresolved.customer_id;
        END LOOP;
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS secret_workspaces_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0057_secret_workspaces')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0058_secret_lease_workspaces: dynamic-secret leases live in the workspace Zetta of their mount
-- (lake#362; docs/api/secrets.md#dynamic-leases). Each lease is a sealed record of
-- lake_secrets_dynamic_leases in its mount's workspace, keyed by the lease id: role, the generated
-- credential sealed with the mount data key (unchanged since 0057), expiry, renewals, revocation.
-- Lake no longer writes or reads secrets_dynamic_leases, which the operator's one-shot copy
-- (copy_workspace_records, collection secrets_dynamic_leases) moves and verifies.
--
-- secret_lease_routes is what stays central, and it is dispatch only: which workspace holds a
-- lease (so renew and revoke, which name only the lease, reach it without asking every
-- workspace) and when the expiry loop must next look at it. It names no role, value or
-- ciphertext. due_at is a hint that is never later than the expiry in the workspace record; the
-- loop claims due rows with FOR UPDATE SKIP LOCKED, settles each lease in its workspace with a
-- conditional write, and sets due_at to NULL once the lease is revoked or expired there.
--
-- A legacy lease follows its mount: the mount's route (secret_mount_routes) names its workspace,
-- and the copy writes its route and record. A lease whose mount has no route is reported here and
-- waits for its mount's assignment. Nothing is dropped; a later migration drops
-- secrets_dynamic_leases once the copy has been verified on every environment.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS secret_lease_routes (
    lease_id     uuid        PRIMARY KEY,
    mount_id     uuid        NOT NULL REFERENCES secret_mount_routes(mount_id) ON DELETE CASCADE,
    customer_id  uuid        NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    workspace_id uuid        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    due_at       timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS secret_lease_routes_due_idx
    ON secret_lease_routes (due_at) WHERE due_at IS NOT NULL;

-- A route names a live workspace of its own space; neither column changes afterwards.
DROP TRIGGER IF EXISTS secret_lease_route_workspace_routing ON secret_lease_routes;
CREATE TRIGGER secret_lease_route_workspace_routing BEFORE INSERT OR UPDATE ON secret_lease_routes
    FOR EACH ROW EXECUTE FUNCTION enforce_workspace_routing();

DO $$
DECLARE
    unresolved record;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM schema_migrations
                   WHERE version = '0058_secret_lease_workspaces')
       AND to_regclass('secrets_dynamic_leases') IS NOT NULL THEN
        FOR unresolved IN
            SELECT l.id, l.mount_id FROM secrets_dynamic_leases l
            WHERE NOT EXISTS (SELECT 1 FROM secret_mount_routes r WHERE r.mount_id = l.mount_id)
        LOOP
            RAISE WARNING 'dynamic secret lease not copied (needs_selection): lease=%, mount=%',
                unresolved.id, unresolved.mount_id;
        END LOOP;
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS secret_lease_workspaces_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0058_secret_lease_workspaces')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0059_application_settings_workspaces: an application's settings (its name, allowed origins,
-- redirect URIs and hosted-page theme) live in the workspace Zetta of the application's workspace
-- (lake#351; auth_applications.workspace_id, 0049; docs/api/apps.md, docs/architecture/
-- workspace-records.md), one sealed record per application in lake_auth_applications. Lake no
-- longer writes or reads those four columns; the operator's one-shot copy
-- (copy_workspace_records, collection auth_applications) moves each application's settings into
-- its workspace and verifies them.
--
-- auth_applications stays as the content-free routing row that finds an application before its
-- workspace is read: id, customer_id (its space), pool_id (the pool its sessions name),
-- custom_sign_in_domain (the host a hosted page is requested on, unique across lake),
-- workspace_id (0049), created_at and updated_at. New rows leave the four settings columns NULL:
-- they lose NOT NULL and their defaults here, and a row without a name holds none of them. A row
-- with a name is one whose settings the copy moves.
--
-- Each application that still holds settings has its owner recorded in workspace_assignments
-- under object_table auth_application_settings (0049 already recorded the applications' routing
-- under auth_applications): its application's workspace (application_workspace), needs_selection
-- while it has none, or workspace_deleted when that workspace is gone (its records went with it).
-- Nothing is dropped here; a later migration drops the four columns once the copy has been
-- verified on every environment.
-- ---------------------------------------------------------------------------------------------
-- 0070_drop_moved_tables drops the four columns, and a fresh database never has them.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns
                WHERE table_schema = current_schema() AND table_name = 'auth_applications'
                  AND column_name = 'name') THEN
        ALTER TABLE auth_applications ALTER COLUMN name DROP NOT NULL;
        ALTER TABLE auth_applications ALTER COLUMN allowed_origins DROP NOT NULL;
        ALTER TABLE auth_applications ALTER COLUMN redirect_uris DROP NOT NULL;
        ALTER TABLE auth_applications ALTER COLUMN redirect_uris DROP DEFAULT;
        ALTER TABLE auth_applications ALTER COLUMN theme DROP NOT NULL;
        ALTER TABLE auth_applications ALTER COLUMN theme DROP DEFAULT;
        ALTER TABLE auth_applications DROP CONSTRAINT IF EXISTS auth_applications_settings_check;
        ALTER TABLE auth_applications ADD CONSTRAINT auth_applications_settings_check CHECK (
            name IS NOT NULL OR (allowed_origins IS NULL AND redirect_uris IS NULL AND theme IS NULL));
    END IF;
END $$;

DO $$
DECLARE
    unresolved record;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM schema_migrations
                   WHERE version = '0059_application_settings_workspaces')
       AND EXISTS (SELECT 1 FROM information_schema.columns
                      WHERE table_schema = current_schema() AND table_name = 'auth_applications'
                        AND column_name = 'name') THEN
        INSERT INTO workspace_assignments (object_table, object_id, customer_id, workspace_id, outcome)
            SELECT 'auth_application_settings', a.id, a.customer_id,
                   CASE WHEN w.desired = 'deleted' THEN NULL ELSE a.workspace_id END,
                   CASE WHEN a.workspace_id IS NULL THEN 'needs_selection'
                        WHEN w.desired = 'deleted' THEN 'workspace_deleted'
                        ELSE 'application_workspace' END
            FROM auth_applications a LEFT JOIN workspaces w ON w.id = a.workspace_id
            WHERE a.name IS NOT NULL
            ON CONFLICT DO NOTHING;
        FOR unresolved IN SELECT * FROM workspace_assignments WHERE workspace_id IS NULL
                           AND object_table = 'auth_application_settings' LOOP
            RAISE WARNING 'application settings not copied (%): application=%, customer=%',
                unresolved.outcome, unresolved.object_id, unresolved.customer_id;
        END LOOP;
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS application_settings_workspaces_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0059_application_settings_workspaces')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0060_platform_workspaces: repository environments and the platform mirror live in the workspace
-- Zetta of their repository's workspace (lake#358, lake#359, docs/api/platform.md,
-- docs/architecture/workspace-records.md). Lake no longer writes or reads platform_environments or
-- platform_mirror; the operator's one-shot copy (copy_workspace_records) moves each row into its
-- workspace and verifies it.
--
-- An environment's id is now md5(repository id, newline, name) as a UUID, which lake computes the
-- same way. Its `environment` policy object stays here with its grants, and takes that id as its
-- external id, so lake finds it from the repository and the name without reading the workspace.
-- An object is renamed only when no other environment object already has that id.
--
-- Each row's owner is its repository's workspace (repositories.workspace_id, 0051), recorded in
-- workspace_assignments as repository_workspace: an environment under its row id, a mirror row
-- under md5(kind, newline, key). A row whose repository has no workspace yet is needs_selection and
-- is reported; the copy assigns it once the repository is assigned. A row whose repository's
-- workspace was deleted is workspace_deleted: it went with that workspace and does not hold the
-- copy back. A mirror row naming no repository lake has is reported and not assigned. Nothing is
-- dropped here; a later migration drops both tables once the copy has been verified on every
-- environment.
-- ---------------------------------------------------------------------------------------------
DO $$
DECLARE
    unresolved record;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '0060_platform_workspaces')
       AND to_regclass('platform_environments') IS NOT NULL THEN
        UPDATE objects o
           SET external_id = md5(e.repository_id::text || E'\n' || e.name)::uuid::text,
               updated_at = now()
          FROM platform_environments e
         WHERE o.id = e.object_id
           AND o.external_id <> md5(e.repository_id::text || E'\n' || e.name)::uuid::text
           AND NOT EXISTS (SELECT 1 FROM objects t
                            WHERE t.object_type = 'environment'
                              AND t.external_id = md5(e.repository_id::text || E'\n' || e.name)::uuid::text);
        INSERT INTO workspace_assignments (object_table, object_id, customer_id, workspace_id, outcome)
            SELECT 'platform_environments', e.id, r.customer_id,
                   CASE WHEN w.desired = 'deleted' THEN NULL ELSE r.workspace_id END,
                   CASE WHEN r.workspace_id IS NULL THEN 'needs_selection'
                        WHEN w.desired = 'deleted' THEN 'workspace_deleted'
                        ELSE 'repository_workspace' END
            FROM platform_environments e JOIN repositories r ON r.id = e.repository_id
                 LEFT JOIN workspaces w ON w.id = r.workspace_id
            ON CONFLICT DO NOTHING;
        INSERT INTO workspace_assignments (object_table, object_id, customer_id, workspace_id, outcome)
            SELECT 'platform_mirror', md5(m.kind || E'\n' || m.key)::uuid, r.customer_id,
                   CASE WHEN w.desired = 'deleted' THEN NULL ELSE r.workspace_id END,
                   CASE WHEN r.workspace_id IS NULL THEN 'needs_selection'
                        WHEN w.desired = 'deleted' THEN 'workspace_deleted'
                        ELSE 'repository_workspace' END
            FROM platform_mirror m JOIN repositories r ON r.id::text = m.value->>'repository'
                 LEFT JOIN workspaces w ON w.id = r.workspace_id
            ON CONFLICT DO NOTHING;
        FOR unresolved IN SELECT * FROM workspace_assignments
                           WHERE workspace_id IS NULL
                             AND object_table IN ('platform_environments', 'platform_mirror') LOOP
            RAISE WARNING 'platform row not copied (%): table=%, object=%, customer=%',
                unresolved.outcome, unresolved.object_table, unresolved.object_id,
                unresolved.customer_id;
        END LOOP;
        FOR unresolved IN SELECT m.kind, m.key FROM platform_mirror m
                           WHERE NOT EXISTS (SELECT 1 FROM repositories r
                                              WHERE r.id::text = m.value->>'repository') LOOP
            RAISE WARNING 'platform mirror row not copied (unknown_repository): kind=%, key=%',
                unresolved.kind, unresolved.key;
        END LOOP;
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS platform_workspaces_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0060_platform_workspaces')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0061_application_authorization_code_workspaces: an application's one-use OIDC authorization
-- codes (lake#363) live in the workspace Zetta of the application's workspace
-- (auth_applications.workspace_id, 0049; docs/api/apps.md, docs/architecture/workspace-records.md),
-- in the collection lake_auth_app_authorization_codes: issued with if_absent, redeemed by a delete
-- at the revision read, so a code is redeemed at most once, and expired by the collection's
-- five-minute retention. Lake no longer writes or reads auth_app_authorization_codes; the
-- operator's one-shot copy (copy_workspace_records) moves each live code into that workspace and
-- verifies it. (app_authorization_codes, lake's own protected-app forward-auth handoff, is
-- multitenancy and stays.)
--
-- Only live codes (unused, unexpired) have an owner to record; used and expired codes are never
-- copied. Each live code's owner is recorded in workspace_assignments under md5(code_hash) as a
-- UUID: the application's workspace (application_workspace), needs_selection while the application
-- has none (reported, and resolved by the copy once the application is assigned), or
-- workspace_deleted when that workspace is gone. Nothing is dropped here; a later migration drops
-- the table once the copy has been verified on every environment.
-- ---------------------------------------------------------------------------------------------
DO $$
DECLARE
    unresolved record;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM schema_migrations
                   WHERE version = '0061_application_authorization_code_workspaces')
       AND to_regclass('auth_app_authorization_codes') IS NOT NULL THEN
        INSERT INTO workspace_assignments (object_table, object_id, customer_id, workspace_id, outcome)
            SELECT 'auth_app_authorization_codes', md5(c.code_hash)::uuid, a.customer_id,
                   CASE WHEN w.desired = 'deleted' THEN NULL ELSE a.workspace_id END,
                   CASE WHEN a.workspace_id IS NULL THEN 'needs_selection'
                        WHEN w.desired = 'deleted' THEN 'workspace_deleted'
                        ELSE 'application_workspace' END
            FROM auth_app_authorization_codes c
                 JOIN auth_applications a ON a.id = c.application_id
                 LEFT JOIN workspaces w ON w.id = a.workspace_id
            WHERE c.used_at IS NULL AND c.expires_at > now()
            ON CONFLICT DO NOTHING;
        FOR unresolved IN SELECT * FROM workspace_assignments WHERE workspace_id IS NULL
                           AND object_table = 'auth_app_authorization_codes' LOOP
            RAISE WARNING 'application authorization code not copied (%): object=%, customer=%',
                unresolved.outcome, unresolved.object_id, unresolved.customer_id;
        END LOOP;
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS application_authorization_code_workspaces_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0061_application_authorization_code_workspaces')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0062_agent_task_workspaces: what a person or an agent wrote for an agent task (its request body,
-- the agent's messages and its answer) lives in the workspace Zetta of the task's workspace
-- (lake#374, lake#375; agent_tasks.workspace_id, 0038; docs/api/agent-bus.md, docs/architecture/
-- workspace-records.md), one sealed record per task in lake_agent_tasks. Lake no longer writes or
-- reads agent_tasks.body, agent_task_status.message or agent_task_status.result; the operator's
-- one-shot copy (copy_workspace_records, collection agent_tasks) moves each task's content into
-- its workspace and verifies it.
--
-- agent_tasks and agent_task_status stay as the content-free dispatch rows the agent bus, the
-- claim and its lease (0042) use without reading a workspace: ids, space, workspace, agent, kind,
-- conversation, scopes, deadline, who asked, phase, the claim's credential hash, lease, attempts
-- and the pending-notice flag. Two columns are added:
--   agent_tasks.parent_task_id: the task whose agent asked for this one (lake#451), which used to
--     be read out of the body; it routes a delegated turn's progress to the asking turn.
--   agent_task_status.notice: lake's own notice for a task it reclaimed or failed (a retry, a
--     final failure after three expiries, or nobody picking it up), never text a person or an
--     agent wrote. It outranks the agent's message until the agent's next event clears it.
-- Existing rows get both from what they hold. New rows leave body '{}', message '' and result
-- null.
--
-- Each task that still holds content has its owner recorded in workspace_assignments under
-- object_table agent_task_content (0038 recorded the tasks' routing under agent_tasks): its own
-- workspace (task_workspace), needs_selection when it names none, or workspace_deleted when that
-- workspace is gone (its records went with it). Nothing is dropped here; a later migration clears
-- or drops the three content columns once the copy has been verified on every environment.
-- ---------------------------------------------------------------------------------------------
ALTER TABLE agent_tasks ADD COLUMN IF NOT EXISTS parent_task_id uuid;
ALTER TABLE agent_task_status ADD COLUMN IF NOT EXISTS notice text NOT NULL DEFAULT '';

DO $$
DECLARE
    unresolved record;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '0062_agent_task_workspaces')
       AND EXISTS (SELECT 1 FROM information_schema.columns
                      WHERE table_schema = current_schema() AND table_name = 'agent_tasks'
                        AND column_name = 'body') THEN
        UPDATE agent_tasks SET parent_task_id = (body->>'parent_task_id')::uuid
            WHERE parent_task_id IS NULL
              AND body->>'parent_task_id' ~*
                  '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$';
        -- Lake's own notices, as the bus loop wrote them into message (0042).
        UPDATE agent_task_status SET notice = message
            WHERE notice = ''
              AND (message LIKE 'the agent stopped responding%'
                   OR message LIKE 'No agent picked this up within % seconds.');
        INSERT INTO workspace_assignments (object_table, object_id, customer_id, workspace_id, outcome)
            SELECT 'agent_task_content', t.id, t.customer_id,
                   CASE WHEN w.desired <> 'deleted' THEN w.id END,
                   CASE WHEN t.workspace_id IS NULL OR w.id IS NULL THEN 'needs_selection'
                        WHEN w.desired = 'deleted' THEN 'workspace_deleted'
                        ELSE 'task_workspace' END
            FROM agent_tasks t LEFT JOIN agent_task_status ts ON ts.task_id = t.id
                 LEFT JOIN workspaces w ON w.id = t.workspace_id AND w.customer_id = t.customer_id
            WHERE t.body <> '{}'::jsonb OR COALESCE(ts.message, '') <> ''
                  OR COALESCE(ts.result, 'null'::jsonb) <> 'null'::jsonb
            ON CONFLICT DO NOTHING;
        FOR unresolved IN SELECT * FROM workspace_assignments WHERE workspace_id IS NULL
                           AND object_table = 'agent_task_content' LOOP
            RAISE WARNING 'agent task content not copied (%): task=%, customer=%',
                unresolved.outcome, unresolved.object_id, unresolved.customer_id;
        END LOOP;
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS agent_task_workspaces_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0062_agent_task_workspaces')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0063_audit_workspaces: lake's audit splits by subject (decision 52, lake#317, lake#318,
-- docs/api/audit.md#which-log). Control-plane and identity events stay in this table's central
-- hash chain. Events about a workspace's data go to that workspace's own audit, the sealed
-- collection lake_audit_events of its Zetta, chained there; lake no longer writes them here.
--
-- The events recorded here before the split that are about a workspace's data are copied there
-- by the operator's one-shot copy (copy_workspace_records, collection audit_events), keeping
-- their id, signature and central predecessor. Their owners are recorded in workspace_assignments
-- (object_table 'audit_events'): an event whose action is one of the workspace capabilities below
-- (or a push, a branch protection decision or a deployment phase), and that was not refused for
-- want of authority, goes to the workspace it names, found from its details (`workspace_id`,
-- `repository_id`, `repository`) or its object (a repository, app, build, secret mount or agent
-- task). event_workspace: copied there. workspace_deleted: that workspace is gone, so the event
-- stays here. Every other event stays here, and the count is reported: control-plane events, and
-- workspace events that do not name their workspace (most capability events recorded before the
-- split name only `capability/<name>`). Nothing moves out of this table
-- here; audit_events is append-only, and a later migration may replace a copied row's content by
-- a digest-only row so the central chain still verifies.
--
-- audit_retention stays: it governs this central log (lake#318). A workspace's audit is kept
-- until the workspace is deleted (the collection's retention_ms is 0).
-- ---------------------------------------------------------------------------------------------
DO $$
DECLARE
    tally record;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '0063_audit_workspaces') THEN
        INSERT INTO workspace_assignments (object_table, object_id, customer_id, workspace_id, outcome)
            SELECT 'audit_events', r.id, w.customer_id,
                   CASE WHEN w.desired = 'deleted' THEN NULL ELSE w.id END,
                   CASE WHEN w.desired = 'deleted' THEN 'workspace_deleted'
                        ELSE 'event_workspace' END
            FROM (SELECT e.id, e.customer_id, COALESCE(
                      CASE WHEN e.details->>'workspace_id' ~ '^[0-9a-f-]{36}$'
                           THEN (e.details->>'workspace_id')::uuid END,
                      (SELECT p.workspace_id FROM repositories p
                        WHERE p.id = substring(e.object FROM '^repository/([0-9a-f-]{36})')::uuid),
                      (SELECT p.workspace_id FROM repositories p
                        WHERE p.id::text = COALESCE(e.details->>'repository_id',
                                                    e.details->>'repository')),
                      (SELECT a.workspace_id FROM apps a
                        WHERE a.id = substring(e.object FROM '^app/([0-9a-f-]{36})')::uuid),
                      (SELECT b.workspace_id FROM builds b
                        WHERE b.id = substring(e.object FROM '^build/([0-9a-f-]{36})')::uuid),
                      (SELECT m.workspace_id FROM secret_mount_routes m
                        WHERE m.mount_id =
                              substring(e.object FROM '^secret_mount/([0-9a-f-]{36})')::uuid),
                      (SELECT t.workspace_id FROM agent_tasks t
                        WHERE t.id = substring(e.object FROM '^agent_task/([0-9a-f-]{36})')::uuid))
                      AS workspace_id
                  FROM audit_events e
                  WHERE e.action = ANY (ARRAY[
            'agent_turn', 'app_logs', 'application_jwks', 'attach_model_provenance',
            'bind_service_to_app', 'browse_stream', 'build_app', 'build_image', 'build_logs',
            'cancel_build', 'change_diff', 'claim_agent_task', 'commit_repository_files',
            'compare_runs', 'confirm_solution', 'connect_service', 'create_agent_from_proposal',
            'create_app', 'create_application_organization', 'create_application_webhook',
            'create_domain', 'create_drive_folder', 'create_drive_share_link',
            'create_encryption_key', 'create_issue', 'create_issue_comment', 'create_pipeline',
            'create_projects_from_opportunities', 'create_release', 'create_repository',
            'create_review', 'create_review_comment', 'create_secret_mount', 'create_service',
            'create_service_and_bind_to_app', 'create_store', 'create_stream', 'create_webhook',
            'decrypt', 'delete_app', 'delete_application_organization', 'delete_application_user',
            'delete_domain', 'delete_drive_path', 'delete_repository', 'delete_secret_versions',
            'delete_service', 'delete_store', 'delete_stream', 'delete_stream_messages',
            'deliver_webhook', 'deploy_app', 'deployment_phase', 'derive_encryption_key',
            'destroy_secret_versions', 'diff_drive_versions', 'dismiss_opportunity',
            'download_drive_file', 'download_drive_share_link', 'drive_browse', 'drive_conflict',
            'drive_effective_access', 'drive_history', 'drive_preview', 'edit_pipeline',
            'encrypt', 'environment_logs', 'export_application_user', 'export_repository',
            'find_missing', 'get_agent_task', 'get_app', 'get_application', 'get_artifact',
            'get_ask_conversation', 'get_build', 'get_change', 'get_dataset',
            'get_encryption_key', 'get_environment', 'get_experiment_analysis',
            'get_experiment_study', 'get_model', 'get_pipeline', 'get_repository',
            'get_run_metrics', 'get_secret_mount', 'get_stream', 'hmac',
            'import_github_repository', 'import_repository', 'issue_application_jwt',
            'launch_agent', 'list_agent_launches', 'list_agent_schedules',
            'list_application_organizations', 'list_application_users',
            'list_application_webhook_events', 'list_application_webhooks', 'list_apps',
            'list_artifacts', 'list_branch_protections', 'list_builds', 'list_changes',
            'list_conflicts', 'list_datasets', 'list_deployments', 'list_encryption_keys',
            'list_environments', 'list_experiment_trials', 'list_experiments', 'list_exports',
            'list_issue_comments', 'list_issues', 'list_opportunities', 'list_releases',
            'list_repositories', 'list_review_comments', 'list_reviews', 'list_runs',
            'list_secret_mounts', 'list_stores', 'list_streams', 'list_webhook_deliveries',
            'list_webhooks', 'map_review_anchors', 'move_drive_path', 'predict',
            'prepare_repository_upload', 'preview_drive_share_link', 'promote_trial',
            'publish_agent_event', 'publish_app_event', 'publish_build_event',
            'publish_stream_message', 'pull', 'push', 'push_change', 'put_secret', 'read_chunk',
            'read_secret', 'receive_agent_events', 'receive_agent_tasks', 'receive_app_events',
            'receive_build_events', 'reference_update', 'refs', 'register_application',
            'reindex_artifacts', 'rename_repository', 'replicate_repository', 'repository_file',
            'repository_refs', 'repository_tree', 'resolve_conflict', 'resolve_dataset_file',
            'resolve_drive_share_link', 'resolve_model_file', 'restart_app',
            'restore_drive_version', 'retry_application_webhook', 'retry_webhook_delivery',
            'revoke_drive_share_link', 'rewrap', 'rollback_app', 'rotate_application_signing_key',
            'rotate_encryption_key', 'run_pipeline', 'scale_app', 'schedule_agent',
            'search_index', 'secret_metadata', 'service_connection', 'set_app_auth',
            'set_app_env', 'set_app_zetta_binding', 'set_branch_protection',
            'set_environment_env', 'set_repository_visibility', 'share_drive_folder', 'sign',
            'stage_repository_file', 'start_app', 'start_solution', 'start_study',
            'stop_agent_launch', 'stop_app', 'stop_experiment_trial', 'stop_study',
            'store_connection', 'suspend_application_user', 'tail_experiment_events',
            'transfer_repository', 'unbind_service_from_app', 'undelete_secret_versions',
            'unschedule_agent', 'unset_app_env', 'unset_environment_env',
            'update_application_organization', 'update_application_theme', 'update_issue',
            'update_review', 'update_stream', 'update_stream_message', 'upload_chunks',
            'upload_drive_files', 'verify', 'verify_model_provenance', 'workspace_act',
            'workspace_openapi', 'workspace_query', 'workspace_sql'])
                    AND (e.outcome <> 'denied' OR e.kind LIKE 'forge.branch_protection.%')) r
            JOIN workspaces w ON w.id = r.workspace_id
                             AND (r.customer_id IS NULL OR w.customer_id = r.customer_id)
            ON CONFLICT DO NOTHING;
        FOR tally IN SELECT outcome, count(*) AS events FROM workspace_assignments
                      WHERE object_table = 'audit_events' GROUP BY outcome ORDER BY outcome LOOP
            RAISE WARNING 'audit events (%): %', tally.outcome, tally.events;
        END LOOP;
        RAISE WARNING 'audit events staying central: %',
            (SELECT count(*) FROM audit_events e
              WHERE NOT EXISTS (SELECT 1 FROM workspace_assignments a
                                 WHERE a.object_table = 'audit_events' AND a.object_id = e.id));
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS audit_workspaces_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0063_audit_workspaces')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0064_repository_records: the repository row is routing only (decision 54, S10 and S11).
--
-- repository_refs is dropped (lake#319). It was a copy of each liquid ref that lake wrote after a
-- push, a replication and a `refs` read, and that nothing read: the `refs` and `repository_refs`
-- capabilities read the liquid repository, which is the authority and moves with lake#380. It
-- never removed a ref liquid deleted. Its CREATE (0006) is removed above, so a rerun of this file
-- leaves it dropped. Nothing is copied.
--
-- A repository's description lives in the workspace Zetta of the repository's workspace
-- (lake#315 part 2; repositories.workspace_id, 0051; docs/api/repositories.md, docs/architecture/
-- workspace-records.md), in its settings record in lake_repositories, beside any later setting.
-- Lake no longer writes or reads repositories.description; new rows leave it ''. The operator's
-- one-shot copy (copy_workspace_records, collection repositories) moves each description into
-- its workspace and verifies it. The row stays central as routing: id, customer_id (its space),
-- workspace_id, name, visibility, deleted_at, retention_until and the timestamps, which lake needs
-- for a lookup by name, an anonymous read of a public repository and the retention collector
-- before it reads any workspace.
--
-- Each repository that holds a description has its owner recorded in workspace_assignments under
-- object_table repository_descriptions (0051 recorded the repositories' routing under
-- repositories): its repository's workspace (repository_workspace), needs_selection while it has
-- none (reported, and resolved by the copy once the repository is assigned, lake#513), or
-- workspace_deleted when that workspace is gone. A later migration drops the description column
-- once the copy has been verified on every environment.
-- ---------------------------------------------------------------------------------------------
DROP TABLE IF EXISTS repository_refs;

DO $$
DECLARE
    unresolved record;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '0064_repository_records')
       AND EXISTS (SELECT 1 FROM information_schema.columns
                      WHERE table_schema = current_schema() AND table_name = 'repositories'
                        AND column_name = 'description') THEN
        INSERT INTO workspace_assignments (object_table, object_id, customer_id, workspace_id, outcome)
            SELECT 'repository_descriptions', r.id, r.customer_id,
                   CASE WHEN w.desired = 'deleted' THEN NULL ELSE r.workspace_id END,
                   CASE WHEN r.workspace_id IS NULL THEN 'needs_selection'
                        WHEN w.desired = 'deleted' THEN 'workspace_deleted'
                        ELSE 'repository_workspace' END
            FROM repositories r LEFT JOIN workspaces w ON w.id = r.workspace_id
            WHERE r.description <> ''
            ON CONFLICT DO NOTHING;
        FOR unresolved IN SELECT * FROM workspace_assignments WHERE workspace_id IS NULL
                           AND object_table = 'repository_descriptions' LOOP
            RAISE WARNING 'repository description not copied (%): repository=%, customer=%',
                unresolved.outcome, unresolved.object_id, unresolved.customer_id;
        END LOOP;
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS repository_records_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0064_repository_records')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0065_workspace_deletions: deleting a workspace cascades (decision 54 S12, lake#514,
-- docs/api/workspaces.md#deletion). The transaction that sets a workspace's desired state to
-- `deleted` also tombstones every repository of the workspace still live or inside its retention
-- with its retention ended (deleted_at kept where set, retention_until = now(), which releases
-- its name), deletes the workspace's apps, cancels its unfinished builds, and records here what
-- it destroyed, with the tenant image repositories that existed then.
--
-- The rest lives outside PostgreSQL: the tags and manifests of those image repositories in the
-- instance registry (until lake#383 moves them), the destroyed repositories' search segments,
-- and one central audit event per destroyed object (the workspace's own audit chain goes with its
-- Zetta). Lake does it after the commit and sets completed_at; a deletion without completed_at
-- is resumed by lake's owner loop and by a repeated delete_workspace, and every step is
-- idempotent. Repository descriptions (lake_repositories, 0064) and every other lake record in
-- the workspace Zetta go with the Zetta. The operator still deletes the workspace Zetta, and with it the workspace's record
-- store, from the workspace's desired state; it does not read this table.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS workspace_deletions (
    workspace_id   uuid        PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    customer_id    uuid        NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    workspace_name text        NOT NULL,
    requested_by   text        NOT NULL,
    requested_at   timestamptz NOT NULL DEFAULT now(),
    -- Each an array of {"id", "name", "state"}: repositories (state live | tombstoned), apps
    -- (their desired state before), builds (their phase when cancelled), images (state tagged).
    repositories   jsonb       NOT NULL DEFAULT '[]'::jsonb,
    apps           jsonb       NOT NULL DEFAULT '[]'::jsonb,
    builds         jsonb       NOT NULL DEFAULT '[]'::jsonb,
    images         jsonb       NOT NULL DEFAULT '[]'::jsonb,
    completed_at   timestamptz
);
CREATE INDEX IF NOT EXISTS workspace_deletions_pending_idx ON workspace_deletions (requested_at)
    WHERE completed_at IS NULL;

CREATE TABLE IF NOT EXISTS workspace_deletions_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0065_workspace_deletions')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0066_assign_unrouted_repositories: the reviewed assignment 0051 left to a later migration
-- (decision 54 S14, lake#513, docs/api/repositories.md#the-owning-workspace). Each repository
-- that still names no workspace (live or tombstoned) is assigned its space's only live workspace
-- when the space now has exactly one; a space with several or none is left alone and reported
-- with RAISE WARNING. It runs once. On dev it assigns the `liquid` repository to the workspace
-- created for its space.
--
-- The routing trigger refuses any change of workspace_id. Rather than drop it, as 0051 did,
-- enforce_workspace_routing honors one exception: on repositories, NULL to a live workspace of
-- the same space while the transaction-local setting lake.repository_assignment is
-- '0066_assign_unrouted_repositories'. This block sets it, makes the updates and clears it, so
-- the trigger refuses every other change before, during (for any other row or table) and after.
--
-- Each assignment is recorded in workspace_assignments: the repository's routing row
-- (object_table repositories) changes from needs_selection to sole_workspace, and the owner of
-- its description, if 0064 recorded one, from needs_selection to repository_workspace, the
-- outcome copy_workspace_records gives it. The other needs_selection rows that follow a
-- repository's workspace (forge records, platform environments) are resolved by their copy.
-- ---------------------------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION enforce_workspace_routing() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF TG_TABLE_NAME = 'repositories' AND OLD.workspace_id IS NULL
           AND NEW.workspace_id IS NOT NULL AND NEW.customer_id IS NOT DISTINCT FROM OLD.customer_id
           AND current_setting('lake.repository_assignment', true)
               = '0066_assign_unrouted_repositories' THEN
            -- The reviewed assignment of migration 0066: checked as an insert is, below.
            NULL;
        ELSIF NEW.workspace_id IS DISTINCT FROM OLD.workspace_id OR
           NEW.customer_id IS DISTINCT FROM OLD.customer_id THEN
            RAISE EXCEPTION 'workspace_id and customer_id are immutable' USING ERRCODE = '23514';
        ELSE
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
    candidate record;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM schema_migrations
                   WHERE version = '0066_assign_unrouted_repositories') THEN
        PERFORM set_config('lake.repository_assignment', '0066_assign_unrouted_repositories', true);
        FOR candidate IN
            SELECT r.id, r.customer_id, r.name, count(w.id) AS live,
                   (array_agg(w.id ORDER BY w.created_at))[1] AS workspace_id
            FROM repositories r
                 LEFT JOIN workspaces w ON w.customer_id = r.customer_id AND w.desired <> 'deleted'
            WHERE r.workspace_id IS NULL
            GROUP BY r.id, r.customer_id, r.name
            ORDER BY r.id
        LOOP
            IF candidate.live <> 1 THEN
                RAISE WARNING 'workspace_id requires selection: table=repositories, object=%, customer=%, live_workspaces=%',
                    candidate.id, candidate.customer_id, candidate.live;
                CONTINUE;
            END IF;
            UPDATE repositories SET workspace_id = candidate.workspace_id
                WHERE id = candidate.id AND workspace_id IS NULL;
            INSERT INTO workspace_assignments (object_table, object_id, customer_id, workspace_id,
                                               outcome)
                VALUES ('repositories', candidate.id, candidate.customer_id,
                        candidate.workspace_id, 'sole_workspace')
                ON CONFLICT (object_table, object_id) DO UPDATE
                    SET workspace_id = EXCLUDED.workspace_id, outcome = EXCLUDED.outcome,
                        recorded_at = now()
                    WHERE workspace_assignments.workspace_id IS NULL;
            UPDATE workspace_assignments
                SET workspace_id = candidate.workspace_id, outcome = 'repository_workspace',
                    recorded_at = now()
                WHERE object_table = 'repository_descriptions' AND object_id = candidate.id
                  AND workspace_id IS NULL AND outcome = 'needs_selection';
            RAISE NOTICE 'repository assigned its sole workspace: repository=% (%), customer=%, workspace=%',
                candidate.id, candidate.name, candidate.customer_id, candidate.workspace_id;
        END LOOP;
        PERFORM set_config('lake.repository_assignment', '', true);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS assign_unrouted_repositories_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0066_assign_unrouted_repositories')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0067_workspace_store: a workspace's liquid store moves into its workspace Zetta (decision 54
-- S1-S3, lake#380, docs/architecture/workspace-store.md). The bytes (packs, index runs, repository
-- state, all encrypted with the workspace's data key) live in bucket `lake_store` of the workspace
-- Zetta; PostgreSQL keeps only this content-free switch. While store_moved_at is NULL the
-- workspace's repositories are served from its space's store on lake's volume
-- (<LAKE_STORE_ROOT>/<space>/store); move_workspace_store `switch` copies what is new, verifies
-- every repository and sets it, after which they are served from the workspace store and the
-- space's store stays on the volume, read-only, until Steve approves removing it. The operator
-- does not read the column.
-- ---------------------------------------------------------------------------------------------
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS store_moved_at timestamptz;

CREATE TABLE IF NOT EXISTS workspace_store_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0067_workspace_store')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0068_oci_repository_routes: the registry splits (decision 54 S4-S6, lake#383 phase A,
-- docs/api/registry.md#where-a-tenant-image-lives). A tenant OCI repository (`<space>/<name>`,
-- `<space>/cache/<name>`) belongs to one workspace: its manifests and tags are sealed records in
-- that workspace Zetta's lake_oci_manifests and lake_oci_tags, and this table is the content-free
-- route lake resolves before it reads any workspace, so an image keeps its address. A route holds
-- only the space, the repository name below it and the workspace, with its timestamps: no
-- manifest, digest or tag. It is written when a build first pushes its own target (`<image>` or
-- `cache/<image>`, from the build row's workspace) or by create_image_repository (an explicit
-- workspace_id); there is no default workspace. A route never changes
-- (enforce_oci_repository_route); the cascade that deletes a workspace (0065) deletes its routes,
-- and the operator's unroute_image_repository removes a wrong one. npm, Python, debuginfod and legacy one-component OCI
-- repositories stay with the instance and have no route. Blobs stay in the instance registry
-- until phase B (lake#380) moves them into the workspace store.
--
-- Each tenant repository whose owner is unambiguous gets its route here: a name that the apps and
-- builds of exactly one live workspace of the space use is routed there, both `<name>` and
-- `cache/<name>`. A name several workspaces use is reported (RAISE WARNING) and routed by an
-- operator with create_image_repository. The registry's files are not visible to SQL, so the
-- operator's one-shot copy (copy_workspace_records, collection oci_tags) routes the repositories it
-- finds there by the same rule, then by the space's only live workspace, and reports the rest as
-- unresolved.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS oci_repository_routes (
    customer_id  uuid        NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    name         text        NOT NULL CHECK (length(regexp_replace(name, '^cache/', '')) <= 190 AND name ~
                     '^(cache/)?[a-z0-9]([a-z0-9._-]*[a-z0-9])?$'),
    workspace_id uuid        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (customer_id, name)
);
CREATE INDEX IF NOT EXISTS oci_repository_routes_workspace_idx
    ON oci_repository_routes (workspace_id);

CREATE OR REPLACE FUNCTION enforce_oci_repository_route() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF NEW.customer_id IS DISTINCT FROM OLD.customer_id OR NEW.name IS DISTINCT FROM OLD.name
           OR NEW.workspace_id IS DISTINCT FROM OLD.workspace_id THEN
            RAISE EXCEPTION 'an image route never changes' USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM workspaces WHERE id = NEW.workspace_id AND customer_id = NEW.customer_id
            AND desired <> 'deleted'
    ) THEN
        RAISE EXCEPTION 'workspace_id must name a live workspace in this space' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

DROP TRIGGER IF EXISTS oci_repository_route_routing ON oci_repository_routes;
CREATE TRIGGER oci_repository_route_routing BEFORE INSERT OR UPDATE ON oci_repository_routes
    FOR EACH ROW EXECUTE FUNCTION enforce_oci_repository_route();

DO $$
DECLARE
    candidate record;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '0068_oci_repository_routes') THEN
        FOR candidate IN
            WITH used AS (
                SELECT a.customer_id, a.workspace_id, a.name FROM apps a
                WHERE a.desired <> 'deleted'
                UNION
                SELECT b.customer_id, b.workspace_id,
                       substring(b.target FROM '^[^/]+/[^/]+/([^/:]+):[^/:]+$')
                FROM builds b JOIN customers c ON c.id = b.customer_id
                WHERE b.workspace_id IS NOT NULL AND split_part(b.target, '/', 2) = c.name
            )
            SELECT u.customer_id, u.name, count(DISTINCT u.workspace_id) AS owners,
                   (array_agg(DISTINCT u.workspace_id))[1] AS workspace_id
            FROM used u JOIN workspaces w ON w.id = u.workspace_id
                 AND w.customer_id = u.customer_id AND w.desired <> 'deleted'
            WHERE u.name ~ '^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$' AND length(u.name) <= 190
            GROUP BY u.customer_id, u.name
            ORDER BY u.customer_id, u.name
        LOOP
            IF candidate.owners <> 1 THEN
                RAISE WARNING 'image repository needs a workspace: customer=%, name=%, workspaces=%',
                    candidate.customer_id, candidate.name, candidate.owners;
                CONTINUE;
            END IF;
            INSERT INTO oci_repository_routes (customer_id, name, workspace_id)
                VALUES (candidate.customer_id, candidate.name, candidate.workspace_id),
                       (candidate.customer_id, 'cache/' || candidate.name, candidate.workspace_id)
                ON CONFLICT DO NOTHING;
        END LOOP;
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS oci_repository_routes_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0068_oci_repository_routes')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0069_workspace_volumes: the operator's volumes (decision 54 S13, operator#259, operator#260,
-- lake#380, docs/control-plane/schema.md#workspace-volumes-migration-0069_workspace_volumes).
-- After lake#380 a workspace Zetta's volume holds the workspace's liquid store, its tenant images
-- and its search index, and on a one-node workspace it is the only copy. These are desired state
-- and observations only: the operator grows each workspace Zetta's claims and takes CSI snapshots
-- from them, and it never talks to a workspace Zetta (decision 51).
--
-- workspaces.storage_gi is the desired size of each of the workspace Zetta's volumes, in GiB. It
-- never decreases (enforce_workspace_storage_growth): an online-expanded block volume cannot
-- shrink. The operator creates the OramaZettaCluster at this size, grows each existing claim in
-- place, and reports storage_gi_observed (the smallest claim capacity it sees, in GiB),
-- storage_phase (Pending | Resizing | Ready | Failed) and storage_message on the same row, as it
-- reports the workspace's phase.
--
-- volume_snapshot_policies is content-free: a target (`store`, lake's own store volume, or a
-- workspace id), a five-field cron schedule in UTC, how many snapshots to keep, and requested_at,
-- a one-off request the operator honors when it is newer than the target's last snapshot. Every
-- workspace gets the default policy when it is created; lake changes one only through
-- request_volume_snapshot. volume_snapshot_status is written only by the operator: the target's
-- last VolumeSnapshot, whether it is readyToUse, and when it was taken. Neither table has a
-- foreign key to workspaces: a deleted workspace's retained snapshots, and what the operator last
-- reported about them, outlive it (decision 54 S12).
-- ---------------------------------------------------------------------------------------------
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS storage_gi integer NOT NULL DEFAULT 5;
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS storage_gi_observed integer NOT NULL DEFAULT 0;
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS storage_phase text NOT NULL DEFAULT '';
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS storage_message text NOT NULL DEFAULT '';
ALTER TABLE workspaces DROP CONSTRAINT IF EXISTS workspaces_storage_gi_check;
ALTER TABLE workspaces ADD CONSTRAINT workspaces_storage_gi_check
    CHECK (storage_gi BETWEEN 1 AND 32767);
ALTER TABLE workspaces DROP CONSTRAINT IF EXISTS workspaces_storage_gi_observed_check;
ALTER TABLE workspaces ADD CONSTRAINT workspaces_storage_gi_observed_check
    CHECK (storage_gi_observed >= 0);
ALTER TABLE workspaces DROP CONSTRAINT IF EXISTS workspaces_storage_phase_check;
ALTER TABLE workspaces ADD CONSTRAINT workspaces_storage_phase_check
    CHECK (storage_phase IN ('', 'Pending', 'Resizing', 'Ready', 'Failed'));

CREATE OR REPLACE FUNCTION enforce_workspace_storage_growth() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.storage_gi < OLD.storage_gi THEN
        RAISE EXCEPTION 'workspace storage never decreases: % GiB is below % GiB',
            NEW.storage_gi, OLD.storage_gi USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

DROP TRIGGER IF EXISTS workspace_storage_growth ON workspaces;
CREATE TRIGGER workspace_storage_growth BEFORE UPDATE OF storage_gi ON workspaces
    FOR EACH ROW EXECUTE FUNCTION enforce_workspace_storage_growth();

CREATE TABLE IF NOT EXISTS volume_snapshot_policies (
    target       text        PRIMARY KEY CHECK (target = 'store' OR target ~
                     '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    -- Minute, hour, day of month, month, day of week, in UTC. The default is daily at 03:00.
    schedule     text        NOT NULL DEFAULT '0 3 * * *' CHECK (schedule ~
                     '^[0-9*/,-]+ [0-9*/,-]+ [0-9*/,-]+ [0-9*/,-]+ [0-9*/,-]+$'),
    keep         integer     NOT NULL DEFAULT 7 CHECK (keep BETWEEN 1 AND 100),
    requested_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- Written only by the operator: one row per target, replaced on every observed change.
CREATE TABLE IF NOT EXISTS volume_snapshot_status (
    target        text        PRIMARY KEY,
    snapshot_name text        NOT NULL DEFAULT '',
    ready_to_use  boolean     NOT NULL DEFAULT false,
    snapshot_at   timestamptz,
    message       text        NOT NULL DEFAULT '',
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE OR REPLACE FUNCTION add_workspace_snapshot_policy() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO volume_snapshot_policies (target) VALUES (NEW.id::text) ON CONFLICT DO NOTHING;
    RETURN NEW;
END $$;

DROP TRIGGER IF EXISTS workspace_snapshot_policy ON workspaces;
CREATE TRIGGER workspace_snapshot_policy AFTER INSERT ON workspaces
    FOR EACH ROW EXECUTE FUNCTION add_workspace_snapshot_policy();

CREATE OR REPLACE FUNCTION notify_lake_volume_snapshots() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('lake_volume_snapshots', NEW.target);
    RETURN NEW;
END $$;

DROP TRIGGER IF EXISTS lake_volume_snapshots_changed ON volume_snapshot_policies;
CREATE TRIGGER lake_volume_snapshots_changed AFTER INSERT OR UPDATE ON volume_snapshot_policies
    FOR EACH ROW EXECUTE FUNCTION notify_lake_volume_snapshots();

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '0069_workspace_volumes') THEN
        INSERT INTO volume_snapshot_policies (target) VALUES ('store') ON CONFLICT DO NOTHING;
        INSERT INTO volume_snapshot_policies (target)
            SELECT id::text FROM workspaces WHERE desired <> 'deleted' ORDER BY created_at, id
            ON CONFLICT DO NOTHING;
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS workspace_volumes_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0069_workspace_volumes')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0070_drop_moved_tables: the retired copies of workspace data are dropped (lake#311, decision 54).
-- Each moved into the workspace Zetta of its owner through the operator's one-shot copy
-- (copy_workspace_records, docs/architecture/workspace-records.md), which the coordinator reports
-- was run and verified on dev for every collection below, and lake reads and writes none of them:
--   opportunities (0046, lake#376); stream_owners (0047, lake#379); drive_share_links (0048,
--   lake#350; drive_share_link_routes stays); auth_app_webhook_events and
--   auth_app_webhook_deliveries (0049, lake#356, lake#357); metering_usage and
--   metering_build_records (0053, lake#348, lake#349); forge_records (0054, lake#341);
--   auth_app_users and auth_app_organizations (0055, lake#353, lake#354); auth_app_signing_keys and
--   auth_app_webhooks (0056, lake#352, lake#355); secrets_mounts, secrets_entries,
--   secrets_versions, secrets_encryption_keys and secrets_data_keys (0057, lake#328-#332;
--   secret_mount_routes stays); secrets_dynamic_leases (0058, lake#362; secret_lease_routes
--   stays); platform_environments and platform_mirror (0060, lake#358, lake#359; each
--   environment's policy object stays); auth_app_authorization_codes (0061, lake#363);
--   and the content columns of three routing rows: auth_applications name, allowed_origins,
--   redirect_uris and theme (0059, lake#351), agent_tasks.body with agent_task_status.message and
--   result (0062, lake#374, lake#375), and repositories.description (0064, lake#315 part 2).
-- Nothing else is dropped. What stays central by decision stays: identity (50), the operator's
-- desired state (51), audit_events (52: the split copied the events about a workspace's data and
-- left the central chain intact, so no row or column of it is removed), and the keystore, agent
-- sessions and SSO logs (53). The workspace_assignments rows the earlier migrations recorded
-- stay: they are the record of where each row went.
--
-- The first block refuses the whole migration while any row of a table about to be dropped has
-- no recorded workspace owner: no workspace_assignments row with a workspace, unless its outcome
-- says the workspace was deleted (workspace_deleted, or deleted_workspace as 0046 spelled it).
-- Mirror rows naming no repository, expired codes and usage rows without transfer do not count;
-- for the secrets tables, a mount without a route counts. The check proves an owner was
-- recorded, not that a copy ran: copy_workspace_records plan, copy and verify, with a lake image
-- built before this migration, are a manual gate before applying it. When it refuses, either run
-- that copy for each table named with the previous image (oramasearch/lake:20261005a), resolving
-- its unresolved rows, and apply again; or, on a development environment and only with the
-- owner's approval, delete the named rows and apply again. Every statement is idempotent and the
-- earlier CREATEs and migration blocks no longer make these tables or columns, so a rerun of this
-- file leaves them dropped.
-- ---------------------------------------------------------------------------------------------
DO $$
DECLARE
    open_rows text[] := '{}';
    n bigint;
BEGIN
    IF to_regclass('opportunities') IS NOT NULL THEN
        SELECT count(*) INTO n FROM opportunities o
            LEFT JOIN workspace_assignments a ON a.object_table = 'opportunities'
                 AND a.object_id = o.id
            WHERE a.workspace_id IS NULL
              AND COALESCE(a.outcome, '') NOT IN ('workspace_deleted', 'deleted_workspace');
        IF n > 0 THEN open_rows := open_rows || format('opportunities=%s', n); END IF;
    END IF;
    IF to_regclass('stream_owners') IS NOT NULL THEN
        SELECT count(*) INTO n FROM stream_owners s
            LEFT JOIN workspace_assignments a ON a.object_table = 'stream_owners'
                 AND a.object_id = md5(s.customer_id::text || E'\n' || s.workspace_id || E'\n' ||
                                       s.topic)::uuid
            WHERE a.workspace_id IS NULL AND COALESCE(a.outcome, '') NOT IN ('workspace_deleted', 'deleted_workspace');
        IF n > 0 THEN open_rows := open_rows || format('stream_owners=%s', n); END IF;
    END IF;
    IF to_regclass('drive_share_links') IS NOT NULL THEN
        SELECT count(*) INTO n FROM drive_share_links l
            LEFT JOIN workspace_assignments a ON a.object_table = 'drive_share_links'
                 AND a.object_id = l.id
            WHERE a.workspace_id IS NULL
              AND COALESCE(a.outcome, '') NOT IN ('workspace_deleted', 'deleted_workspace');
        IF n > 0 THEN open_rows := open_rows || format('drive_share_links=%s', n); END IF;
    END IF;
    IF to_regclass('auth_app_webhook_events') IS NOT NULL THEN
        SELECT count(*) INTO n FROM auth_app_webhook_events e
            LEFT JOIN workspace_assignments a ON a.object_table = 'auth_app_webhook_events'
                 AND a.object_id = e.id
            WHERE a.workspace_id IS NULL AND COALESCE(a.outcome, '') NOT IN ('workspace_deleted', 'deleted_workspace');
        IF n > 0 THEN open_rows := open_rows || format('auth_app_webhook_events=%s', n); END IF;
    END IF;
    IF to_regclass('metering_usage') IS NOT NULL THEN
        SELECT count(*) INTO n FROM metering_usage m
            LEFT JOIN workspace_assignments a ON a.object_table = 'metering_usage'
                 AND a.object_id = md5(m.customer_id::text || E'\n' ||
                     to_char(m.hour AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'))::uuid
            WHERE (m.transfer_in_bytes > 0 OR m.transfer_out_bytes > 0)
              AND a.workspace_id IS NULL AND COALESCE(a.outcome, '') NOT IN ('workspace_deleted', 'deleted_workspace');
        IF n > 0 THEN open_rows := open_rows || format('metering_usage=%s', n); END IF;
    END IF;
    IF to_regclass('metering_build_records') IS NOT NULL THEN
        SELECT count(*) INTO n FROM metering_build_records b
            LEFT JOIN workspace_assignments a ON a.object_table = 'metering_build_records'
                 AND a.object_id = md5(b.customer_id::text || E'\n' || b.result_id)::uuid
            WHERE a.workspace_id IS NULL AND COALESCE(a.outcome, '') NOT IN ('workspace_deleted', 'deleted_workspace');
        IF n > 0 THEN open_rows := open_rows || format('metering_build_records=%s', n); END IF;
    END IF;
    IF to_regclass('forge_records') IS NOT NULL THEN
        SELECT count(*) INTO n FROM forge_records f
            LEFT JOIN workspace_assignments a ON a.object_table = 'forge_records'
                 AND a.object_id = f.id
            WHERE a.workspace_id IS NULL AND COALESCE(a.outcome, '') NOT IN ('workspace_deleted', 'deleted_workspace');
        IF n > 0 THEN open_rows := open_rows || format('forge_records=%s', n); END IF;
    END IF;
    IF to_regclass('auth_app_users') IS NOT NULL THEN
        SELECT count(*) INTO n FROM auth_app_users u
            LEFT JOIN workspace_assignments a ON a.object_table = 'auth_app_users'
                 AND a.object_id = md5(u.application_id::text || E'\n' || u.user_id::text)::uuid
            WHERE a.workspace_id IS NULL AND COALESCE(a.outcome, '') NOT IN ('workspace_deleted', 'deleted_workspace');
        IF n > 0 THEN open_rows := open_rows || format('auth_app_users=%s', n); END IF;
    END IF;
    IF to_regclass('auth_app_organizations') IS NOT NULL THEN
        SELECT count(*) INTO n FROM auth_app_organizations o
            LEFT JOIN workspace_assignments a ON a.object_table = 'auth_app_organizations'
                 AND a.object_id = o.id
            WHERE a.workspace_id IS NULL AND COALESCE(a.outcome, '') NOT IN ('workspace_deleted', 'deleted_workspace');
        IF n > 0 THEN open_rows := open_rows || format('auth_app_organizations=%s', n); END IF;
    END IF;
    IF to_regclass('auth_app_signing_keys') IS NOT NULL THEN
        SELECT count(DISTINCT k.application_id) INTO n FROM auth_app_signing_keys k
            LEFT JOIN workspace_assignments a ON a.object_table = 'auth_app_signing_keys'
                 AND a.object_id = k.application_id
            WHERE a.workspace_id IS NULL AND COALESCE(a.outcome, '') NOT IN ('workspace_deleted', 'deleted_workspace');
        IF n > 0 THEN open_rows := open_rows || format('auth_app_signing_keys=%s', n); END IF;
    END IF;
    IF to_regclass('auth_app_webhooks') IS NOT NULL THEN
        SELECT count(*) INTO n FROM auth_app_webhooks h
            LEFT JOIN workspace_assignments a ON a.object_table = 'auth_app_webhooks'
                 AND a.object_id = h.id
            WHERE a.workspace_id IS NULL AND COALESCE(a.outcome, '') NOT IN ('workspace_deleted', 'deleted_workspace');
        IF n > 0 THEN open_rows := open_rows || format('auth_app_webhooks=%s', n); END IF;
    END IF;
    IF to_regclass('auth_app_authorization_codes') IS NOT NULL THEN
        SELECT count(*) INTO n FROM auth_app_authorization_codes c
            LEFT JOIN workspace_assignments a ON a.object_table = 'auth_app_authorization_codes'
                 AND a.object_id = md5(c.code_hash)::uuid
            WHERE c.used_at IS NULL AND c.expires_at > now()
              AND a.workspace_id IS NULL AND COALESCE(a.outcome, '') NOT IN ('workspace_deleted', 'deleted_workspace');
        IF n > 0 THEN
            open_rows := open_rows || format('auth_app_authorization_codes=%s', n);
        END IF;
    END IF;
    -- Every secrets row goes where its mount's route says (0057); a lease names its mount too.
    IF to_regclass('secrets_mounts') IS NOT NULL THEN
        SELECT count(*) INTO n FROM secrets_mounts m
            LEFT JOIN workspace_assignments a ON a.object_table = 'secrets_mounts'
                 AND a.object_id = m.id
            WHERE NOT EXISTS (SELECT 1 FROM secret_mount_routes r WHERE r.mount_id = m.id)
              AND COALESCE(a.outcome, '') NOT IN ('workspace_deleted', 'deleted_workspace');
        IF n > 0 THEN open_rows := open_rows || format('secrets_mounts=%s', n); END IF;
    END IF;
    IF to_regclass('secrets_dynamic_leases') IS NOT NULL THEN
        SELECT count(*) INTO n FROM secrets_dynamic_leases l
            LEFT JOIN workspace_assignments a ON a.object_table = 'secrets_mounts'
                 AND a.object_id = l.mount_id
            WHERE NOT EXISTS (SELECT 1 FROM secret_mount_routes r WHERE r.mount_id = l.mount_id)
              AND COALESCE(a.outcome, '') NOT IN ('workspace_deleted', 'deleted_workspace');
        IF n > 0 THEN open_rows := open_rows || format('secrets_dynamic_leases=%s', n); END IF;
    END IF;
    IF to_regclass('platform_environments') IS NOT NULL THEN
        SELECT count(*) INTO n FROM platform_environments e
            LEFT JOIN workspace_assignments a ON a.object_table = 'platform_environments'
                 AND a.object_id = e.id
            WHERE a.workspace_id IS NULL AND COALESCE(a.outcome, '') NOT IN ('workspace_deleted', 'deleted_workspace');
        IF n > 0 THEN open_rows := open_rows || format('platform_environments=%s', n); END IF;
    END IF;
    IF to_regclass('platform_mirror') IS NOT NULL THEN
        -- A row naming no repository lake has (unknown_repository) has no owner and does not count.
        SELECT count(*) INTO n FROM platform_mirror m
            JOIN repositories r ON r.id::text = m.value->>'repository'
            LEFT JOIN workspace_assignments a ON a.object_table = 'platform_mirror'
                 AND a.object_id = md5(m.kind || E'\n' || m.key)::uuid
            WHERE a.workspace_id IS NULL AND COALESCE(a.outcome, '') NOT IN ('workspace_deleted', 'deleted_workspace');
        IF n > 0 THEN open_rows := open_rows || format('platform_mirror=%s', n); END IF;
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.columns
                WHERE table_schema = current_schema() AND table_name = 'auth_applications'
                  AND column_name = 'name') THEN
        SELECT count(*) INTO n FROM auth_applications p
            LEFT JOIN workspace_assignments a ON a.object_table = 'auth_application_settings'
                 AND a.object_id = p.id
            WHERE p.name IS NOT NULL
              AND a.workspace_id IS NULL AND COALESCE(a.outcome, '') NOT IN ('workspace_deleted', 'deleted_workspace');
        IF n > 0 THEN open_rows := open_rows || format('auth_application_settings=%s', n); END IF;
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.columns
                WHERE table_schema = current_schema() AND table_name = 'agent_tasks'
                  AND column_name = 'body') THEN
        SELECT count(*) INTO n FROM agent_tasks t
            LEFT JOIN agent_task_status ts ON ts.task_id = t.id
            LEFT JOIN workspace_assignments a ON a.object_table = 'agent_task_content'
                 AND a.object_id = t.id
            WHERE (t.body <> '{}'::jsonb OR COALESCE(ts.message, '') <> ''
                   OR COALESCE(ts.result, 'null'::jsonb) <> 'null'::jsonb)
              AND a.workspace_id IS NULL AND COALESCE(a.outcome, '') NOT IN ('workspace_deleted', 'deleted_workspace');
        IF n > 0 THEN open_rows := open_rows || format('agent_task_content=%s', n); END IF;
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.columns
                WHERE table_schema = current_schema() AND table_name = 'repositories'
                  AND column_name = 'description') THEN
        SELECT count(*) INTO n FROM repositories r
            LEFT JOIN workspace_assignments a ON a.object_table = 'repository_descriptions'
                 AND a.object_id = r.id
            WHERE r.description <> ''
              AND a.workspace_id IS NULL AND COALESCE(a.outcome, '') NOT IN ('workspace_deleted', 'deleted_workspace');
        IF n > 0 THEN open_rows := open_rows || format('repository_descriptions=%s', n); END IF;
    END IF;
    IF cardinality(open_rows) > 0 THEN
        RAISE EXCEPTION '0070_drop_moved_tables: rows without a recorded workspace owner: %',
            array_to_string(open_rows, ', ')
            USING HINT = 'Nothing was dropped. This lake no longer has the copier: either run '
                         'copy_workspace_records plan, copy and verify for each collection named '
                         'with the previous lake image (oramasearch/lake:20261005a or any image '
                         'built before migration 0070), resolve its unresolved rows, and apply '
                         'the schema again; or, on a development environment and only with the '
                         'owner''s approval (it destroys data), delete the named rows and apply '
                         'the schema again.';
    END IF;
END $$;

DROP TABLE IF EXISTS opportunities;
DROP TABLE IF EXISTS stream_owners;
DROP TABLE IF EXISTS drive_share_links;
DROP TABLE IF EXISTS auth_app_webhook_deliveries;
DROP TABLE IF EXISTS auth_app_webhook_events;
DROP TABLE IF EXISTS auth_app_webhooks;
DROP TABLE IF EXISTS auth_app_signing_keys;
DROP TABLE IF EXISTS auth_app_organizations;
DROP TABLE IF EXISTS auth_app_users;
DROP TABLE IF EXISTS auth_app_authorization_codes;
DROP TABLE IF EXISTS metering_usage;
DROP TABLE IF EXISTS metering_build_records;
DROP TABLE IF EXISTS forge_records;
DROP TABLE IF EXISTS secrets_dynamic_leases;
DROP TABLE IF EXISTS secrets_versions;
DROP TABLE IF EXISTS secrets_entries;
DROP TABLE IF EXISTS secrets_encryption_keys;
DROP TABLE IF EXISTS secrets_data_keys;
DROP TABLE IF EXISTS secrets_mounts;
DROP TABLE IF EXISTS platform_mirror;
DROP TABLE IF EXISTS platform_environments;
ALTER TABLE auth_applications DROP CONSTRAINT IF EXISTS auth_applications_settings_check;
ALTER TABLE auth_applications DROP COLUMN IF EXISTS name;
ALTER TABLE auth_applications DROP COLUMN IF EXISTS allowed_origins;
ALTER TABLE auth_applications DROP COLUMN IF EXISTS redirect_uris;
ALTER TABLE auth_applications DROP COLUMN IF EXISTS theme;
ALTER TABLE agent_tasks DROP COLUMN IF EXISTS body;
ALTER TABLE agent_task_status DROP COLUMN IF EXISTS message;
ALTER TABLE agent_task_status DROP COLUMN IF EXISTS result;
ALTER TABLE repositories DROP COLUMN IF EXISTS description;

CREATE TABLE IF NOT EXISTS drop_moved_tables_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0070_drop_moved_tables')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0071_secret_mount_owners (lake#561): a secret mount's owner is the customer of its workspace
-- (an account, such as an individual's personal space) or, as before, an organization inside that
-- customer's namespace. A mount an account owns has no organization, so its route carries
-- organization_id NULL, and its policy object hangs under the customer's object
-- (customer → environment `secrets/<customer>` → secret_mount) instead of an organization's
-- (organization → environment `secrets/<customer>/<organization>` → secret_mount). The route stays
-- routing only: mount, space, owner and workspace, with no name, value or key. Existing rows are
-- untouched; dropping a NOT NULL constraint is idempotent.
-- ---------------------------------------------------------------------------------------------
ALTER TABLE secret_mount_routes ALTER COLUMN organization_id DROP NOT NULL;

CREATE TABLE IF NOT EXISTS secret_mount_owners_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0071_secret_mount_owners')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0072_usage_limits (lake#564): a customer's usage limits, one per dimension of usage
-- (docs/api/metering.md#limits). A limit is a number in the dimension's unit, per UTC calendar
-- month for a counted dimension, and the percentage of it from which lake warns. It is
-- content-free account configuration, like customer_storage_quotas, which still holds the
-- enforced repository storage limit; the usage it is compared with stays in each workspace Zetta.
-- No price, plan or bill is kept here.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS customer_usage_limits (
    customer_id  uuid        NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    dimension    text        NOT NULL CHECK (dimension ~ '^[a-z][a-z_]{0,63}$'),
    limit_value  bigint      NOT NULL CHECK (limit_value > 0),
    warn_percent smallint    NOT NULL DEFAULT 80 CHECK (warn_percent BETWEEN 1 AND 100),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (customer_id, dimension)
);

CREATE TABLE IF NOT EXISTS usage_limits_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0072_usage_limits')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0073_billing (lake#564, decision 56): billing records, in the control plane as multitenancy
-- (docs/api/billing.md). The price list (plans and a price per usage dimension, keyed on the
-- dimension names usage_over_time answers), each account's plan over time, and the invoices lake
-- issues with their line items and payment state. Usage itself stays in each workspace Zetta: a
-- line item freezes only the quantity billed for one dimension over one period. No workspace
-- content is kept here. Lake writes these rows only where billing is on (LAKE_BILLING=on).
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS billing_plans (
    id          text        PRIMARY KEY CHECK (id ~ '^[a-z][a-z0-9-]{0,62}$'),
    name        text        NOT NULL CHECK (length(name) BETWEEN 1 AND 128),
    description text        NOT NULL DEFAULT '' CHECK (length(description) <= 1024),
    currency    text        NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    -- The plan's fee for one UTC calendar month, in millionths of the currency's unit.
    base_micros bigint      NOT NULL DEFAULT 0 CHECK (base_micros >= 0),
    active      boolean     NOT NULL DEFAULT true,
    position    integer     NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- One price per usage dimension of a plan: `unit_micros` for every `per` units of the dimension's
-- unit beyond `included` units a month (overage). A dimension without a row is not charged.
CREATE TABLE IF NOT EXISTS billing_plan_prices (
    plan_id     text   NOT NULL REFERENCES billing_plans(id) ON DELETE CASCADE,
    dimension   text   NOT NULL CHECK (dimension ~ '^[a-z][a-z_]{0,63}$'),
    unit_micros bigint NOT NULL CHECK (unit_micros >= 0),
    per         bigint NOT NULL DEFAULT 1 CHECK (per > 0),
    included    bigint NOT NULL DEFAULT 0 CHECK (included >= 0),
    PRIMARY KEY (plan_id, dimension)
);

-- An account's plan from the first day of a month: the plan of a month is the row with the latest
-- `effective_from` on or before its first day. A change chosen mid-month starts the next month.
CREATE TABLE IF NOT EXISTS billing_subscriptions (
    customer_id    uuid        NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    effective_from date        NOT NULL CHECK (extract(day FROM effective_from) = 1),
    plan_id        text        NOT NULL REFERENCES billing_plans(id),
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (customer_id, effective_from)
);

-- An issued invoice: one account, one month, one plan, and its payment state. An invoice is
-- kept when its account is deleted (customer_id is not a foreign key): it is a financial record.
CREATE TABLE IF NOT EXISTS billing_invoices (
    id                 uuid        PRIMARY KEY,
    number             bigserial   UNIQUE,
    customer_id        uuid        NOT NULL,
    period             text        NOT NULL CHECK (period ~ '^[0-9]{4}-(0[1-9]|1[0-2])$'),
    plan_id            text        NOT NULL,
    plan_name          text        NOT NULL,
    currency           text        NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    total_micros       bigint      NOT NULL CHECK (total_micros >= 0),
    state              text        NOT NULL DEFAULT 'due' CHECK (state IN ('due', 'paid', 'void')),
    provider           text        NOT NULL CHECK (provider ~ '^[a-z][a-z0-9_]{0,31}$'),
    provider_reference text        NOT NULL DEFAULT '' CHECK (length(provider_reference) <= 256),
    instructions       text        NOT NULL DEFAULT '' CHECK (length(instructions) <= 1024),
    note               text        NOT NULL DEFAULT '' CHECK (length(note) <= 1024),
    issued_at          timestamptz NOT NULL DEFAULT now(),
    paid_at            timestamptz,
    voided_at          timestamptz
);
-- At most one invoice an account and month that is not void.
CREATE UNIQUE INDEX IF NOT EXISTS billing_invoices_one_per_period
    ON billing_invoices (customer_id, period) WHERE state <> 'void';
CREATE INDEX IF NOT EXISTS billing_invoices_by_customer
    ON billing_invoices (customer_id, period DESC);

-- An invoice's lines: the plan's fee, then one line per priced dimension with the quantity used
-- in the period (the dimension's unit), what the plan includes, what is billed beyond it, the
-- price and the amount. Numbers only.
CREATE TABLE IF NOT EXISTS billing_invoice_lines (
    invoice_id    uuid    NOT NULL REFERENCES billing_invoices(id) ON DELETE CASCADE,
    position      integer NOT NULL CHECK (position >= 0),
    kind          text    NOT NULL CHECK (kind IN ('plan', 'usage')),
    dimension     text    NOT NULL DEFAULT '' CHECK (dimension ~ '^([a-z][a-z_]{0,63})?$'),
    description   text    NOT NULL CHECK (length(description) BETWEEN 1 AND 256),
    unit          text    NOT NULL DEFAULT '' CHECK (length(unit) <= 32),
    quantity      bigint  NOT NULL DEFAULT 0 CHECK (quantity >= 0),
    included      bigint  NOT NULL DEFAULT 0 CHECK (included >= 0),
    billable      bigint  NOT NULL DEFAULT 0 CHECK (billable >= 0),
    unit_micros   bigint  NOT NULL DEFAULT 0 CHECK (unit_micros >= 0),
    per           bigint  NOT NULL DEFAULT 1 CHECK (per > 0),
    amount_micros bigint  NOT NULL CHECK (amount_micros >= 0),
    PRIMARY KEY (invoice_id, position)
);

CREATE TABLE IF NOT EXISTS billing_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0073_billing')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0074_payment_provider (lake#620, decision 57): Stripe as the payment provider
-- (docs/api/billing.md#stripe). The provider's id for each account (a Stripe Customer), kept once;
-- where an invoice is paid (Stripe's hosted invoice page) and the provider's own state of it; and
-- every provider notice (a Stripe webhook event) applied, by event id, so one is never applied
-- twice. Ids, states and URLs only: no key, no card, no workspace content. Idempotent.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS billing_accounts (
    customer_id          uuid        NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    provider             text        NOT NULL CHECK (provider ~ '^[a-z][a-z0-9_]{0,31}$'),
    provider_customer_id text        NOT NULL CHECK (length(provider_customer_id) BETWEEN 1 AND 255),
    created_at           timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (customer_id, provider)
);

ALTER TABLE billing_invoices ADD COLUMN IF NOT EXISTS payment_url text NOT NULL DEFAULT ''
    CHECK (length(payment_url) <= 2048);
ALTER TABLE billing_invoices ADD COLUMN IF NOT EXISTS provider_status text NOT NULL DEFAULT ''
    CHECK (provider_status ~ '^[a-z_]{0,32}$');
CREATE INDEX IF NOT EXISTS billing_invoices_by_provider_reference
    ON billing_invoices (provider, provider_reference) WHERE provider_reference <> '';

-- A provider's notice, recorded in the transaction that applies it: a second delivery of the
-- same event id changes nothing. Kept like the invoices it moved.
CREATE TABLE IF NOT EXISTS billing_payment_events (
    provider    text        NOT NULL CHECK (provider ~ '^[a-z][a-z0-9_]{0,31}$'),
    event_id    text        NOT NULL CHECK (length(event_id) BETWEEN 1 AND 255),
    event_type  text        NOT NULL DEFAULT '' CHECK (length(event_type) <= 128),
    reference   text        NOT NULL DEFAULT '' CHECK (length(reference) <= 256),
    invoice_id  uuid,
    received_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, event_id)
);

CREATE TABLE IF NOT EXISTS payment_provider_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0074_payment_provider')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0075_workload_resources (lake#643, docs/api/metering.md#resources): compute is metered by the
-- resources each workload is scheduled with, and priced as CPU, RAM, GPU and VRAM. Written only
-- by the Orama operator: one row per workload (a workspace's engine, a managed service or store,
-- an app, an image build, an agent launch) with the totals of its scheduled pods: each
-- container's request, or its limit where it has no request, of CPU (millicores) and memory
-- (bytes), its GPUs (`nvidia.com/gpu`), their class (`l4`, `a10g`, `a100`, `h100`, ...) and their
-- memory. `pods` is how many of its pods are scheduled now; a build's or a job's row keeps what
-- its pods ran with after they finish, with `pods` 0. Numbers only: no workspace content. Lake's
-- owner loop reads them and records the resource-seconds in each workspace Zetta.
--
-- A price may have a class (a GPU class), so a dimension can carry one rate per class beside its
-- default (class ''); an invoice line names the class it bills. Idempotent; drops nothing.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS workload_resources (
    object_kind  text        NOT NULL CHECK (object_kind IN ('workspace', 'service', 'store', 'app',
                                                             'build', 'agent_launch')),
    object_id    uuid        NOT NULL,
    pods         integer     NOT NULL DEFAULT 0 CHECK (pods >= 0),
    cpu_millis   bigint      NOT NULL DEFAULT 0 CHECK (cpu_millis >= 0),
    memory_bytes bigint      NOT NULL DEFAULT 0 CHECK (memory_bytes >= 0),
    gpus         integer     NOT NULL DEFAULT 0 CHECK (gpus >= 0),
    gpu_class    text        NOT NULL DEFAULT '' CHECK (gpu_class ~ '^([a-z0-9][a-z0-9-]{0,31})?$'),
    vram_bytes   bigint      NOT NULL DEFAULT 0 CHECK (vram_bytes >= 0),
    observed_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (object_kind, object_id)
);

ALTER TABLE billing_plan_prices ADD COLUMN IF NOT EXISTS class text NOT NULL DEFAULT ''
    CHECK (class ~ '^([a-z0-9][a-z0-9-]{0,31})?$');
ALTER TABLE billing_plan_prices DROP CONSTRAINT IF EXISTS billing_plan_prices_pkey;
ALTER TABLE billing_plan_prices ADD CONSTRAINT billing_plan_prices_pkey
    PRIMARY KEY (plan_id, dimension, class);
ALTER TABLE billing_invoice_lines ADD COLUMN IF NOT EXISTS class text NOT NULL DEFAULT ''
    CHECK (class ~ '^([a-z0-9][a-z0-9-]{0,31})?$');

CREATE TABLE IF NOT EXISTS workload_resources_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0075_workload_resources')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0076_organization_deletion (lake#645, docs/api/organizations.md#deleting-an-organization): an
-- organization can be deleted once its space holds no workspace. delete_organization removes its
-- teams, memberships, invitations, SSO providers, federation issuers, recovery attempts and secret
-- mount routes and the policy objects of its space and of itself (with every grant on them), and
-- marks the organization and its space deleted. The rows stay, marked, because the central audit
-- chain names the space (audit_events.customer_id is ON DELETE RESTRICT); lake never lists or
-- reads a deleted one again, and its handle stays retired. Idempotent; drops nothing.
-- ---------------------------------------------------------------------------------------------
ALTER TABLE organizations ADD COLUMN IF NOT EXISTS deleted_at timestamptz;
ALTER TABLE customers ADD COLUMN IF NOT EXISTS deleted_at timestamptz;

CREATE TABLE IF NOT EXISTS organization_deletion_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0076_organization_deletion')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0077_metered_jobs (lake#658, docs/api/metering.md#resources): every kind of usage is metered.
-- An agent cron schedule's runs (agent_cron_runs) run pods like a job-mode launch, so the Orama
-- operator reports each run's resources as `agent_cron_run` (object_id the run's id) and keeps
-- them after its pods finish; lake charges each finished build, job launch and cron run once, in
-- the run's workspace Zetta. Numbers only. Idempotent; drops nothing.
-- ---------------------------------------------------------------------------------------------
ALTER TABLE workload_resources DROP CONSTRAINT IF EXISTS workload_resources_object_kind_check;
ALTER TABLE workload_resources ADD CONSTRAINT workload_resources_object_kind_check
    CHECK (object_kind IN ('workspace', 'service', 'store', 'app', 'build', 'agent_launch',
                           'agent_cron_run'));

CREATE TABLE IF NOT EXISTS metered_jobs_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0077_metered_jobs')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0078_methodology_runs (lake#655, docs/api/methodologies.md#runs): a workspace's own
-- methodology is Python code in one of its repositories, and it runs as untrusted code. Lake
-- writes one methodology_runs row per run and NOTIFYs lake_services; the Orama operator waits for
-- the methodology's image build, then runs three pods: `stage` (the pristine runtime image, with
-- the run's credential, egress to Lake only) writes the data bundle to the run's volume, `run`
-- (the built image, no credential, no service account token, no network unless `network`) runs
-- the code, and `collect` (the runtime image, with the credential) posts the result. The
-- operator writes methodology_run_status, including the SHA-256 of the credential it generated
-- for the run (`lmr.<run id>.<secret>`); the credential itself is never stored. Ids, images,
-- resources and phases only: the parameters, data, metrics and artifact live in the workspace
-- Zetta. `workload_resources` gains `methodology_run` (the run pod's resources, kept after it
-- finishes, as a build's are), beside every kind 0077_metered_jobs allows. Idempotent; drops
-- nothing.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS methodology_runs (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id        uuid        NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    workspace_id       uuid        NOT NULL REFERENCES workspaces(id),
    -- The workspace methodology's id in the workspace Zetta's registry (`W-<8 hex>`).
    methodology_id     text        NOT NULL,
    mode               text        NOT NULL,
    -- The methodology's image build; the operator starts the run once it has succeeded.
    build_id           uuid        REFERENCES builds(id) ON DELETE SET NULL,
    -- The built image's pull reference with its digest; empty until known.
    image              text        NOT NULL DEFAULT '',
    -- The pristine methodology-runtime image the stage and collect pods run.
    runtime_image      text        NOT NULL,
    cpu_millis         integer     NOT NULL,
    memory_bytes       bigint      NOT NULL,
    gpus               integer     NOT NULL DEFAULT 0,
    gpu_class          text        NOT NULL DEFAULT '',
    timeout_seconds    integer     NOT NULL,
    -- Whether the code may reach the internet (never cluster, private or metadata addresses).
    network            boolean     NOT NULL DEFAULT false,
    slug               text        NOT NULL UNIQUE,
    desired            text        NOT NULL DEFAULT 'present',
    generation         bigint      NOT NULL DEFAULT 1,
    stop_reason        text        NOT NULL DEFAULT '',
    created_by_user_id uuid        REFERENCES users(id) ON DELETE SET NULL,
    created_by_taap_id text        NOT NULL DEFAULT '',
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT methodology_run_mode_check CHECK (mode IN ('validate', 'train')),
    CONSTRAINT methodology_run_desired_check CHECK (desired IN ('present', 'absent')),
    CONSTRAINT methodology_run_slug_format CHECK (slug ~ '^mr-[0-9a-f]{12}$'),
    CONSTRAINT methodology_run_methodology_format CHECK (methodology_id ~ '^W-[0-9a-f]{8}$'),
    CONSTRAINT methodology_run_resources_check CHECK (
        cpu_millis > 0 AND memory_bytes > 0 AND gpus >= 0 AND timeout_seconds > 0
    ),
    CONSTRAINT methodology_run_gpu_class_check CHECK (gpu_class ~ '^([a-z0-9][a-z0-9-]{0,31})?$')
);
CREATE INDEX IF NOT EXISTS methodology_runs_workspace_idx
    ON methodology_runs (workspace_id, methodology_id, created_at DESC);
CREATE INDEX IF NOT EXISTS methodology_runs_present_idx
    ON methodology_runs (desired) WHERE desired = 'present';
DROP TRIGGER IF EXISTS methodology_run_workspace_routing ON methodology_runs;
CREATE TRIGGER methodology_run_workspace_routing BEFORE INSERT OR UPDATE ON methodology_runs
    FOR EACH ROW EXECUTE FUNCTION enforce_workspace_routing();

-- Written only by the operator: one row per run, replaced on every observed change.
CREATE TABLE IF NOT EXISTS methodology_run_status (
    run_id              uuid        PRIMARY KEY REFERENCES methodology_runs(id) ON DELETE CASCADE,
    phase               text        NOT NULL DEFAULT '',
    message             text        NOT NULL DEFAULT '',
    exit_code           integer,
    credential_sha256   text        NOT NULL DEFAULT '',
    observed_generation bigint      NOT NULL DEFAULT 0,
    -- When the run pod (the untrusted code) started and finished; what lake charges for.
    started_at          timestamptz,
    finished_at         timestamptz,
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT methodology_run_phase_check CHECK (
        phase IN ('', 'Building', 'Staging', 'Running', 'Collecting', 'Succeeded', 'Failed',
                  'Stopped')
    )
);
CREATE INDEX IF NOT EXISTS methodology_run_status_credential_idx
    ON methodology_run_status (credential_sha256) WHERE credential_sha256 <> '';

ALTER TABLE workload_resources DROP CONSTRAINT IF EXISTS workload_resources_object_kind_check;
ALTER TABLE workload_resources ADD CONSTRAINT workload_resources_object_kind_check
    CHECK (object_kind IN ('workspace', 'service', 'store', 'app', 'build', 'agent_launch',
                           'agent_cron_run', 'methodology_run'));

CREATE TABLE IF NOT EXISTS methodology_runs_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0078_methodology_runs')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0079_methodology_run_queue (orama-operator#293, docs/api/methodologies.md#runs): the Orama
-- operator holds a methodology run back, with phase `Queued` and a message saying why, while its
-- workspace already has as many runs staging, running or collecting as the Lake's
-- `spec.methodologies.maxConcurrentRunsPerWorkspace` allows, or while no node of a GPU run's GPU
-- class exists to place its work volume next to. Only the phase CHECK changes. Idempotent; drops
-- nothing.
-- ---------------------------------------------------------------------------------------------
ALTER TABLE methodology_run_status DROP CONSTRAINT IF EXISTS methodology_run_phase_check;
ALTER TABLE methodology_run_status ADD CONSTRAINT methodology_run_phase_check CHECK (
    phase IN ('', 'Building', 'Queued', 'Staging', 'Running', 'Collecting', 'Succeeded', 'Failed',
              'Stopped')
);

CREATE TABLE IF NOT EXISTS methodology_run_queue_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0079_methodology_run_queue')
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------
-- 0080_methodology_deployments (lake#690, docs/api/methodologies.md#serving-its-model): a model
-- a workspace methodology trained (kind `python:W-…`) is served from Ship → Models by its own
-- code. Publishing it writes one methodology_deployments row and NOTIFYs lake_services; the Orama
-- operator runs it in the methodology-runs namespace as a `stage` Job (the pristine runtime image,
-- the deployment's credential, egress to Lake only) that writes the model's artifact to a volume,
-- then a long-running `serve` Deployment (the methodology's image, no credential, no service
-- account token, no network unless `network`, ingress from Lake's pods only) behind a Service.
-- The operator writes methodology_deployment_status, including the SHA-256 of the credential it
-- generated (`lmd.<deployment id>.<secret>`) and the Service's in-cluster `endpoint`, which Lake
-- forwards predictions to. Ids, images, resources and phases only: the model, its artifact and
-- every prediction stay in the workspace Zetta. `workload_resources` gains
-- `methodology_deployment` (the serve pods' resources while they run). Idempotent; drops nothing.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS methodology_deployments (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id        uuid        NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    workspace_id       uuid        NOT NULL REFERENCES workspaces(id),
    -- The workspace methodology (`W-<8 hex>`) and the engine's id of the model it serves.
    methodology_id     text        NOT NULL,
    model_id           text        NOT NULL,
    -- The training run that produced the model; its image build is the serve pod's image.
    run_id             uuid        REFERENCES methodology_runs(id) ON DELETE SET NULL,
    build_id           uuid        REFERENCES builds(id) ON DELETE SET NULL,
    image              text        NOT NULL DEFAULT '',
    runtime_image      text        NOT NULL,
    cpu_millis         integer     NOT NULL,
    memory_bytes       bigint      NOT NULL,
    gpus               integer     NOT NULL DEFAULT 0,
    gpu_class          text        NOT NULL DEFAULT '',
    network            boolean     NOT NULL DEFAULT false,
    slug               text        NOT NULL UNIQUE,
    desired            text        NOT NULL DEFAULT 'present',
    generation         bigint      NOT NULL DEFAULT 1,
    stop_reason        text        NOT NULL DEFAULT '',
    created_by_user_id uuid        REFERENCES users(id) ON DELETE SET NULL,
    created_by_taap_id text        NOT NULL DEFAULT '',
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT methodology_deployment_desired_check CHECK (desired IN ('present', 'absent')),
    CONSTRAINT methodology_deployment_slug_format CHECK (slug ~ '^md-[0-9a-f]{12}$'),
    CONSTRAINT methodology_deployment_methodology_format CHECK (methodology_id ~ '^W-[0-9a-f]{8}$'),
    CONSTRAINT methodology_deployment_model_format CHECK (model_id ~ '^[A-Za-z0-9._:-]{1,128}$'),
    CONSTRAINT methodology_deployment_resources_check CHECK (
        cpu_millis > 0 AND memory_bytes > 0 AND gpus >= 0
    ),
    CONSTRAINT methodology_deployment_gpu_class_check CHECK (
        gpu_class ~ '^([a-z0-9][a-z0-9-]{0,31})?$'
    )
);
-- One present deployment per model of a workspace.
CREATE UNIQUE INDEX IF NOT EXISTS methodology_deployments_present_model_idx
    ON methodology_deployments (workspace_id, model_id) WHERE desired = 'present';
CREATE INDEX IF NOT EXISTS methodology_deployments_workspace_idx
    ON methodology_deployments (workspace_id, created_at DESC);
DROP TRIGGER IF EXISTS methodology_deployment_workspace_routing ON methodology_deployments;
CREATE TRIGGER methodology_deployment_workspace_routing
    BEFORE INSERT OR UPDATE ON methodology_deployments
    FOR EACH ROW EXECUTE FUNCTION enforce_workspace_routing();

-- Written only by the operator: one row per deployment, replaced on every observed change.
CREATE TABLE IF NOT EXISTS methodology_deployment_status (
    deployment_id       uuid        PRIMARY KEY
                                    REFERENCES methodology_deployments(id) ON DELETE CASCADE,
    phase               text        NOT NULL DEFAULT '',
    message             text        NOT NULL DEFAULT '',
    credential_sha256   text        NOT NULL DEFAULT '',
    -- The serve Service's in-cluster URL; empty until the operator has created it.
    endpoint            text        NOT NULL DEFAULT '',
    observed_generation bigint      NOT NULL DEFAULT 0,
    -- When the serve pod first became ready, and when it stopped serving.
    ready_at            timestamptz,
    stopped_at          timestamptz,
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT methodology_deployment_phase_check CHECK (
        phase IN ('', 'Building', 'Queued', 'Staging', 'Starting', 'Ready', 'Failed', 'Stopped')
    ),
    CONSTRAINT methodology_deployment_endpoint_check CHECK (
        endpoint = '' OR endpoint ~ '^http://md-[0-9a-f]{12}\.[a-z0-9-]+\.svc\.cluster\.local(:[0-9]{1,5})?$'
    )
);
CREATE INDEX IF NOT EXISTS methodology_deployment_status_credential_idx
    ON methodology_deployment_status (credential_sha256) WHERE credential_sha256 <> '';

ALTER TABLE workload_resources DROP CONSTRAINT IF EXISTS workload_resources_object_kind_check;
ALTER TABLE workload_resources ADD CONSTRAINT workload_resources_object_kind_check
    CHECK (object_kind IN ('workspace', 'service', 'store', 'app', 'build', 'agent_launch',
                           'agent_cron_run', 'methodology_run', 'methodology_deployment'));

CREATE TABLE IF NOT EXISTS methodology_deployments_guard (
    applied_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0080_methodology_deployments')
    ON CONFLICT DO NOTHING;
