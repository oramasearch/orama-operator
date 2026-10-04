CREATE TABLE IF NOT EXISTS services (
    id uuid PRIMARY KEY,
    generation bigint NOT NULL
);

CREATE TABLE IF NOT EXISTS service_status (
    service_id uuid PRIMARY KEY,
    phase text NOT NULL DEFAULT '',
    message text NOT NULL DEFAULT '',
    endpoint text NOT NULL DEFAULT '',
    observed_generation bigint NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS workspaces (
    id uuid PRIMARY KEY,
    customer_id uuid NOT NULL,
    name text NOT NULL,
    environment text NOT NULL,
    desired text NOT NULL,
    generation bigint NOT NULL,
    phase text,
    observed_generation bigint,
    message text,
    namespace text,
    zetta_service_id uuid,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    storage_gi integer NOT NULL DEFAULT 5,
    storage_gi_observed integer NOT NULL DEFAULT 0,
    storage_phase text NOT NULL DEFAULT '',
    storage_message text NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS volume_snapshot_policies (
    target text PRIMARY KEY,
    schedule text NOT NULL DEFAULT '0 3 * * *',
    keep integer NOT NULL DEFAULT 7,
    requested_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS volume_snapshot_status (
    target text PRIMARY KEY,
    snapshot_name text NOT NULL DEFAULT '',
    ready_to_use boolean NOT NULL DEFAULT false,
    snapshot_at timestamptz,
    message text NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL DEFAULT now()
);
