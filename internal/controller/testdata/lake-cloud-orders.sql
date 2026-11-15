-- The cloud_orders contract between lake and the operator (operator#634, lake#831).
--
-- A verbatim copy of lake's 0089_cloud_orders migration: oramasearch/lake db/schema.sql at commit
-- cde4e90454a2b803c1d0afdc3db206ce84c80a9a
-- (https://github.com/oramasearch/lake/pull/833, branch feat/831-order-sheet), from its
-- "-- 0089_cloud_orders" header through the cloud_orders_paid trigger. Left out: the migration's
-- cloud_orders_guard table and its schema_migrations row, which need lake's migration table.
-- lake owns the table; when its 0089 changes, copy it here again and update the commit above.
-- TestCloudOrdersFixtureMatchesTheStoresSQL checks that every column the store's SQL names is here.
-- 0097 (the entitlement view the operator reads) is appended at the end.

-- ---------------------------------------------------------------------------------------------
-- 0089_cloud_orders (lake#831, docs/api/orders.md, docs/control-plane/schema.md#cloud-orders-migration):
-- the apex order sheet's orders. A private warehouse or dedicated cluster paid through Stripe
-- Checkout starts `pending_payment` and Stripe's signed webhook makes it `paid`; the Orama operator
-- (orama-operator#634) reads `paid` rows, claims one (`claimed_by`, `claimed_at`), and writes
-- `provisioning`, then `ready` with the `host` or `failed` with the `failure_reason`, adding a
-- cloud_order_events row for each change. An order form (any offer, payment skipped) is
-- `submitted`, emailed to support and never provisioned. Contact details and what was asked for,
-- the Stripe ids, the state and its history: nothing from a workspace. Orders are kept (billing
-- records); `organization_id` is set when the person ordering chose an organization (lake#832),
-- with no foreign key, like billing_invoices.customer_id. Lake alone writes the email columns and
-- `audited_at`. Idempotent.
-- ---------------------------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS cloud_orders (
    id                         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at                 timestamptz NOT NULL DEFAULT now(),
    updated_at                 timestamptz NOT NULL DEFAULT now(),
    offer                      text        NOT NULL
        CHECK (offer IN ('private_warehouse', 'dedicated_cluster', 'on_prem', 'shared')),
    path                       text        NOT NULL CHECK (path IN ('checkout', 'order_form')),
    cloud_provider             text        NOT NULL DEFAULT '' CHECK (length(cloud_provider) <= 64),
    region                     text        NOT NULL DEFAULT '' CHECK (length(region) <= 64),
    contact_name               text        NOT NULL CHECK (length(contact_name) BETWEEN 1 AND 200),
    contact_email              text        NOT NULL CHECK (length(contact_email) BETWEEN 3 AND 254),
    company                    text        NOT NULL DEFAULT '' CHECK (length(company) <= 200),
    details                    text        NOT NULL DEFAULT '' CHECK (length(details) <= 4000),
    quantity                   integer     NOT NULL DEFAULT 1 CHECK (quantity BETWEEN 1 AND 1000),
    amount_cents               bigint      NOT NULL CHECK (amount_cents >= 0),
    currency                   text        NOT NULL DEFAULT 'usd' CHECK (currency ~ '^[a-z]{3}$'),
    included_credit_cents      bigint      NOT NULL DEFAULT 0 CHECK (included_credit_cents >= 0),
    stripe_checkout_session_id text        UNIQUE CHECK (length(stripe_checkout_session_id) <= 255),
    stripe_customer_id         text        CHECK (length(stripe_customer_id) <= 255),
    stripe_subscription_id     text        CHECK (length(stripe_subscription_id) <= 255),
    state                      text        NOT NULL CHECK (state IN ('submitted', 'pending_payment',
        'paid', 'provisioning', 'ready', 'failed', 'cancelled')),
    host                       text        CHECK (length(host) <= 253),
    failure_reason             text        CHECK (length(failure_reason) <= 2000),
    claimed_by                 text        CHECK (length(claimed_by) <= 255),
    claimed_at                 timestamptz,
    organization_id            uuid,
    support_emailed_state      text,
    customer_emailed_state     text,
    email_attempts             integer     NOT NULL DEFAULT 0 CHECK (email_attempts >= 0),
    email_error                text        NOT NULL DEFAULT '' CHECK (length(email_error) <= 1024),
    email_retry_at             timestamptz,
    -- Only the two offers lake can take payment for go through Checkout.
    CONSTRAINT cloud_orders_checkout_offer
        CHECK (path = 'order_form' OR offer IN ('private_warehouse', 'dedicated_cluster')),
    -- A paid order is one warehouse or one cluster, with one host; more is an order form.
    CONSTRAINT cloud_orders_checkout_one CHECK (path = 'order_form' OR quantity = 1)
);
-- The operator's read: paid orders, oldest first.
CREATE INDEX IF NOT EXISTS cloud_orders_paid_idx ON cloud_orders (created_at) WHERE state = 'paid';
CREATE INDEX IF NOT EXISTS cloud_orders_email_due_idx ON cloud_orders (email_retry_at)
    WHERE support_emailed_state IS DISTINCT FROM state OR customer_emailed_state IS DISTINCT FROM state;

CREATE TABLE IF NOT EXISTS cloud_order_events (
    id         bigserial   PRIMARY KEY,
    order_id   uuid        NOT NULL REFERENCES cloud_orders(id) ON DELETE CASCADE,
    at         timestamptz NOT NULL DEFAULT now(),
    from_state text        CHECK (from_state IN ('submitted', 'pending_payment', 'paid',
        'provisioning', 'ready', 'failed', 'cancelled')),
    to_state   text        NOT NULL CHECK (to_state IN ('submitted', 'pending_payment', 'paid',
        'provisioning', 'ready', 'failed', 'cancelled')),
    actor      text        NOT NULL CHECK (actor IN ('lake', 'stripe', 'operator')),
    detail     text        NOT NULL DEFAULT '' CHECK (length(detail) <= 2000),
    audited_at timestamptz
);
CREATE INDEX IF NOT EXISTS cloud_order_events_order_idx ON cloud_order_events (order_id, id);
CREATE INDEX IF NOT EXISTS cloud_order_events_unaudited_idx ON cloud_order_events (id)
    WHERE audited_at IS NULL;

-- updated_at follows every write, the operator's included; a paid order is announced on
-- lake_cloud_orders so the operator need not poll.
CREATE OR REPLACE FUNCTION touch_cloud_order() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS cloud_orders_touch ON cloud_orders;
CREATE TRIGGER cloud_orders_touch BEFORE UPDATE ON cloud_orders
    FOR EACH ROW EXECUTE FUNCTION touch_cloud_order();

CREATE OR REPLACE FUNCTION notify_lake_cloud_orders() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.state = 'paid' AND (TG_OP = 'INSERT' OR OLD.state IS DISTINCT FROM NEW.state) THEN
        PERFORM pg_notify('lake_cloud_orders', NEW.id::text);
    END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS cloud_orders_paid ON cloud_orders;
CREATE TRIGGER cloud_orders_paid AFTER INSERT OR UPDATE OF state ON cloud_orders
    FOR EACH ROW EXECUTE FUNCTION notify_lake_cloud_orders();

-- ---------------------------------------------------------------------------------------------
-- 0097_cloud_order_entitlements (lake#972, LSR-105; orama-operator#862): a verbatim copy of lake's
-- migration, oramasearch/lake db/schema.sql at commit 2bb3a7886bf308d69d597b858d2b5e439ef169dc, from its "-- 0097" header
-- through the cloud_order_desired_state view. Left out, as above: its guard table and its
-- schema_migrations row. The operator reads the view, never cloud_orders directly, so every read
-- carries the entitlement as of the statement.
-- ---------------------------------------------------------------------------------------------
-- 0097_cloud_order_entitlements (lake#972): signed subscription snapshots and a clock-evaluated
-- operator contract. Existing subscriptions require a signed renewal/snapshot before provisioning.
ALTER TABLE cloud_orders ADD COLUMN IF NOT EXISTS subscription_status text NOT NULL DEFAULT 'pending'
    CHECK (length(subscription_status) <= 64);
ALTER TABLE cloud_orders ADD COLUMN IF NOT EXISTS paid_through bigint NOT NULL DEFAULT 0
    CHECK (paid_through BETWEEN 0 AND 253402300799);
ALTER TABLE cloud_orders ADD COLUMN IF NOT EXISTS entitlement_access_until bigint NOT NULL DEFAULT 0
    CHECK (entitlement_access_until BETWEEN 0 AND 253402300799);
ALTER TABLE cloud_orders ADD COLUMN IF NOT EXISTS subscription_event_created bigint NOT NULL DEFAULT 0
    CHECK (subscription_event_created BETWEEN 0 AND 253402300799);
ALTER TABLE cloud_orders ADD COLUMN IF NOT EXISTS entitlement_grace_seconds integer NOT NULL DEFAULT 259200
    CHECK (entitlement_grace_seconds BETWEEN 0 AND 2592000);
ALTER TABLE cloud_orders ADD COLUMN IF NOT EXISTS entitlement_retry_invoice_id text NOT NULL DEFAULT ''
    CHECK (length(entitlement_retry_invoice_id) <= 255);
CREATE UNIQUE INDEX IF NOT EXISTS cloud_orders_subscription_unique ON cloud_orders (stripe_subscription_id)
    WHERE stripe_subscription_id IS NOT NULL;

-- Read this view on every reconcile, including already-ready orders. No expiry worker is required.
-- The columns are listed, never `*`: Postgres fixes a view's columns when it is created, so a
-- later `ALTER TABLE cloud_orders ADD COLUMN` would make this CREATE OR REPLACE VIEW try to insert
-- a column before `entitlement_state` and fail the whole schema apply. A later migration that wants
-- a new column in the view appends it after `desired_state`.
CREATE OR REPLACE VIEW cloud_order_desired_state AS
SELECT
    id, created_at, updated_at, offer, path, cloud_provider, region, contact_name, contact_email,
    company, details, quantity, amount_cents, currency, included_credit_cents,
    stripe_checkout_session_id, stripe_customer_id, stripe_subscription_id, state, host,
    failure_reason, claimed_by, claimed_at, organization_id, support_emailed_state,
    customer_emailed_state, email_attempts, email_error, email_retry_at, subscription_status,
    paid_through, entitlement_access_until, subscription_event_created, entitlement_grace_seconds,
    entitlement_retry_invoice_id,
    entitlement_state, CASE
    WHEN path <> 'checkout' THEN 'unmanaged'
    WHEN state IN ('paid', 'provisioning', 'ready') AND entitlement_state IN ('active', 'grace')
        THEN 'present'
    ELSE 'suspended'
END AS desired_state
FROM (
    SELECT
        id, created_at, updated_at, offer, path, cloud_provider, region, contact_name, contact_email,
        company, details, quantity, amount_cents, currency, included_credit_cents,
        stripe_checkout_session_id, stripe_customer_id, stripe_subscription_id, state, host,
        failure_reason, claimed_by, claimed_at, organization_id, support_emailed_state,
        customer_emailed_state, email_attempts, email_error, email_retry_at, subscription_status,
        paid_through, entitlement_access_until, subscription_event_created, entitlement_grace_seconds,
        entitlement_retry_invoice_id,
        CASE
        WHEN path <> 'checkout' THEN 'not_applicable'
        WHEN stripe_subscription_id IS NULL OR stripe_customer_id IS NULL OR entitlement_access_until = 0
            THEN 'suspended'
        WHEN extract(epoch FROM statement_timestamp()) < entitlement_access_until THEN 'active'
        WHEN extract(epoch FROM statement_timestamp()) < entitlement_access_until + entitlement_grace_seconds THEN 'grace'
        ELSE 'suspended'
    END AS entitlement_state
    FROM cloud_orders
) orders;
