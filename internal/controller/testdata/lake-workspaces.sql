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
    updated_at timestamptz NOT NULL DEFAULT now()
);
