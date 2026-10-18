-- The cloud_orders contract between lake and the operator (operator#634, lake#831), for the pgx
-- store's test. Lake owns the real migration; once lake#831 lands, its db/schema.sql is the
-- source of truth and this file must match it in every column the operator reads or writes.
-- Columns the operator must never write (the Stripe ids, the contact, the free text) are here so
-- the test can check that they are left alone.
CREATE TABLE cloud_orders (
    id uuid PRIMARY KEY,
    offer text NOT NULL CHECK (offer IN ('private_warehouse', 'dedicated_cluster', 'on_prem', 'shared')),
    path text NOT NULL CHECK (path IN ('checkout', 'order_form')),
    cloud_provider text,
    region text,
    contact_email text NOT NULL,
    company text,
    notes text,
    stripe_checkout_session_id text,
    stripe_subscription_id text,
    state text NOT NULL CHECK (state IN ('submitted', 'pending_payment', 'paid', 'provisioning', 'ready', 'failed', 'cancelled')),
    host text,
    failure_reason text,
    claimed_by text,
    claimed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE cloud_order_events (
    id bigserial PRIMARY KEY,
    order_id uuid NOT NULL REFERENCES cloud_orders(id),
    at timestamptz NOT NULL DEFAULT now(),
    from_state text,
    to_state text NOT NULL,
    actor text NOT NULL CHECK (actor IN ('lake', 'stripe', 'operator')),
    detail text
);
