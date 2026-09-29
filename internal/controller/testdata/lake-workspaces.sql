CREATE TABLE workspaces (
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
