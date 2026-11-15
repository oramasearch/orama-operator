-- Operator SQL (operator#378, not a dashboard migration): seeds the dashboard's global reference
-- rows (subscription_plans, stripe_addon_prices, llms) with the stack's catalog, IDs and values, so a
-- team can be created and every copied team's plan reference resolves.
--
-- The operator renders this file twice. Inside <cr>-controldb-schema-0008, between 0008's tables and
-- its constraints, so the default plan exists before teams_subscription_plan_id_subscription_plans_id_fk
-- validates existing teams; there the stack's Stripe prices are empty. And as
-- <cr>-controldb-reference-data, after 0008, with the stack's spec.components.controlDB.stripePrices,
-- applied again whenever the rows or prices change.
--
-- Before this file the operator creates and fills two temporary tables from the stack's spec:
--   stack_stripe_plan_prices (id uuid, monthly varchar(255), yearly varchar(255))
--   stack_stripe_addon_prices (id uuid, stripe_price_id varchar(255))
-- The rows come from the stack's spec.components.controlDB.referenceRows, or the built-in example
-- catalog (dashboard-reference-rows.sql) when that is empty, spliced in at the marker line below.
--
-- Each row is upserted by ID to the committed values (ON CONFLICT (id) DO UPDATE): the operator is
-- the only writer of these rows on a stack. Nothing is deleted. An ID in the stack's prices that is
-- not a reference row, or a Stripe price ID already used by another add-on row, fails the whole
-- transaction, visibly, in the migration Job.

CREATE TEMP TABLE reference_subscription_plans (LIKE subscription_plans INCLUDING DEFAULTS) ON COMMIT DROP;
CREATE TEMP TABLE reference_stripe_addon_prices (LIKE stripe_addon_prices INCLUDING DEFAULTS) ON COMMIT DROP;
ALTER TABLE reference_stripe_addon_prices ALTER COLUMN stripe_price_id DROP NOT NULL;
CREATE TEMP TABLE reference_llms (LIKE llms INCLUDING DEFAULTS) ON COMMIT DROP;

-- @reference-rows

DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM stack_stripe_plan_prices s WHERE NOT EXISTS (SELECT 1 FROM reference_subscription_plans r WHERE r.id = s.id)) THEN
  RAISE EXCEPTION 'spec.components.controlDB.stripePrices.plans names a plan that is not a reference row';
 END IF;
 IF EXISTS (SELECT 1 FROM stack_stripe_addon_prices s WHERE NOT EXISTS (SELECT 1 FROM reference_stripe_addon_prices r WHERE r.id = s.id)) THEN
  RAISE EXCEPTION 'spec.components.controlDB.stripePrices.addons names an add-on price that is not a reference row';
 END IF;
END $$;

-- The committed rows carry no Stripe price IDs; this stack's come from its spec.
UPDATE reference_subscription_plans r
SET stripe_price_id_monthly = s.monthly, stripe_price_id_yearly = s.yearly
FROM stack_stripe_plan_prices s WHERE s.id = r.id;
UPDATE reference_stripe_addon_prices r
SET stripe_price_id = s.stripe_price_id
FROM stack_stripe_addon_prices s WHERE s.id = r.id;
DELETE FROM reference_stripe_addon_prices WHERE stripe_price_id IS NULL;

-- Upsert every column of the staged rows into the target table, by ID.
CREATE FUNCTION pg_temp.upsert_reference_rows(target regclass, source regclass) RETURNS void
LANGUAGE plpgsql AS $fn$
DECLARE assignments text;
BEGIN
 SELECT string_agg(format('%I = EXCLUDED.%I', attname, attname), ', ' ORDER BY attnum) INTO assignments
 FROM pg_attribute WHERE attrelid = target AND attnum > 0 AND NOT attisdropped AND attname <> 'id';
 EXECUTE format('INSERT INTO %s SELECT * FROM %s ON CONFLICT (id) DO UPDATE SET %s', target, source, assignments);
END $fn$;

DO $$ BEGIN
 PERFORM pg_temp.upsert_reference_rows('subscription_plans', 'reference_subscription_plans');
 PERFORM pg_temp.upsert_reference_rows('stripe_addon_prices', 'reference_stripe_addon_prices');
 PERFORM pg_temp.upsert_reference_rows('llms', 'reference_llms');
END $$;

-- teams.subscription_plan_id defaults to this plan: without it no team can be created, and 0008's
-- foreign key cannot validate an existing team.
DO $$ BEGIN
 IF NOT EXISTS (SELECT 1 FROM subscription_plans WHERE id = '0d735744-e58e-4438-b618-97523e407cdc') THEN
  RAISE EXCEPTION 'reference rows lack the default subscription plan 0d735744-e58e-4438-b618-97523e407cdc';
 END IF;
END $$;
