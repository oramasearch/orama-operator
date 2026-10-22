-- Runtime privileges are operator-owned deployment policy, separate from Lake's pinned schema.
-- CNPG creates these non-owner roles; the lake owner runs this file in one transaction.
SET LOCAL search_path = pg_catalog, public;
DO $$
DECLARE attempt integer;
BEGIN
    -- CNPG reconciles managed roles asynchronously after the database is ready.
    FOR attempt IN 1..120 LOOP
        EXIT WHEN EXISTS (SELECT FROM pg_roles WHERE rolname = 'lake_runtime')
              AND EXISTS (SELECT FROM pg_roles WHERE rolname = 'lake_operator');
        IF attempt = 120 THEN RAISE EXCEPTION 'waiting for CNPG managed Lake roles'; END IF;
        PERFORM pg_sleep(1);
    END LOOP;
    EXECUTE format('REVOKE ALL ON DATABASE %I FROM PUBLIC, lake_runtime, lake_operator', current_database());
    EXECUTE format('GRANT CONNECT ON DATABASE %I TO lake_runtime, lake_operator', current_database());
END $$;
REVOKE ALL ON SCHEMA public FROM PUBLIC, lake_runtime, lake_operator;
GRANT USAGE ON SCHEMA public TO lake_runtime, lake_operator;
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM PUBLIC, lake_runtime, lake_operator;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM PUBLIC, lake_runtime, lake_operator;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA public FROM PUBLIC, lake_runtime, lake_operator;
-- A table-level REVOKE leaves column ACLs in place. Clear those before rebuilding the policy.
DO $$
DECLARE r record;
BEGIN
    FOR r IN
        SELECT c.relname, string_agg(format('%I', a.attname), ', ' ORDER BY a.attnum) AS cols
          FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
          JOIN pg_attribute a ON a.attrelid = c.oid
         WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
           AND a.attnum > 0 AND NOT a.attisdropped
         GROUP BY c.oid, c.relname
    LOOP
        EXECUTE format('REVOKE ALL (%s) ON public.%I FROM PUBLIC, lake_runtime, lake_operator', r.cols, r.relname);
    END LOOP;
END $$;
ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON TABLES FROM PUBLIC, lake_runtime, lake_operator;
ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON SEQUENCES FROM PUBLIC, lake_runtime, lake_operator;
ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON FUNCTIONS FROM PUBLIC, lake_runtime, lake_operator;

-- An explicit list: new tables get no runtime access until reviewed here. Historical tables
-- removed by later migrations are omitted at apply time. No runtime can edit migration guards.
DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY[
        'users', 'organizations', 'customers', 'repositories', 'objects', 'teams',
        'memberships', 'grants', 'invitations', 'lake_identity', 'audit_events', 'audit_retention',
        'identity_documents', 'devices', 'credentials', 'sessions', 'keystore', 'oauth_providers',
        'identity_oauth_flows', 'identity_webauthn_challenges', 'sso_providers', 'federation_issuers',
        'scim_users', 'scim_groups', 'scim_group_members', 'sso_provisioning_logs',
        'offline_recovery_grants', 'offline_recovery_attempts', 'offline_recovery_admin_approvals',
        'offline_recovery_deliveries', 'offline_recovery_notifications', 'offline_recovery_contacts',
        'pki_authorities', 'pki_certificates', 'pki_acme_orders', 'pki_callers',
        'pki_recovery_ceremonies', 'pki_recovery_shares', 'auth_applications', 'agent_sessions',
        'customer_storage_quotas', 'identity_taap_assertions', 'custodial_identities',
        'services', 'stores', 'agent_launches', 'agent_schedules', 'agent_cron_schedules',
        'agent_tasks', 'agent_task_status', 'workspace_assignments', 'app_authorization_codes',
        'drive_share_link_routes', 'secret_mount_routes', 'secret_lease_routes',
        'workspace_deletions', 'oci_repository_routes', 'volume_snapshot_policies',
        'customer_usage_limits', 'billing_plans', 'billing_plan_prices', 'billing_subscriptions',
        'billing_invoices', 'billing_invoice_lines', 'billing_accounts', 'billing_payment_events',
        'methodology_runs', 'mcp_connection_dispatch', 'store_mounts', 'methodology_deployments',
        'network_connections', 'network_egress_rules', 'network_tunnel_grants', 'network_peers',
        'identity_link_challenges', 'identity_links', 'clerk_webhook_deliveries',
        'cloud_orders', 'cloud_order_events', 'billing_contacts', 'sso_saml_requests',
        'sso_saml_assertions', 'pki_recovery_participants'
    ] LOOP
        IF to_regclass(format('public.%I', t)) IS NOT NULL THEN
            EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON public.%I TO lake_runtime', t);
        END IF;
    END LOOP;
END $$;

-- Observations written by the operator, read by Lake. agent_task_status belongs to Lake's bus.
GRANT SELECT ON public.service_status, public.store_status, public.agent_launch_status,
    public.agent_cron_schedule_status, public.agent_cron_runs, public.volume_snapshot_status,
    public.network_connection_status, public.network_egress_rule_status,
    public.network_tunnel_grant_status, public.network_peer_status TO lake_runtime;

-- Compatibility exceptions pending the companion Lake work tracked by operator#813 (LSR-218):
-- postgres_store.cpp still implements these writers. Move delete acknowledgements to the
-- operator's desired-state pass, and remove Lake's resource/traffic writers before revoking.
GRANT SELECT, INSERT, UPDATE ON public.methodology_run_status,
    public.methodology_deployment_status, public.workload_resources, public.app_transfer TO lake_runtime;

-- Mixed desired/observed tables: runtime writes desired columns; operator writes status columns.
DO $$
DECLARE t text; desired_cols text; status_cols text;
BEGIN
    FOR t, status_cols, desired_cols IN VALUES
        ('apps', 'phase,url,ready,message,observed_generation',
         'id,customer_id,name,mode,source_repository,source_revision,source_path,source_entrypoint,image,framework,port,replicas,host,auth,bindings_json,env_json,secret_env_refs,desired,generation,updated_at,workspace_id'),
        ('builds', 'phase,digest,size,message,token_hashes_json',
         'id,customer_id,repository,revision,context,dockerfile,target,build_args_json,desired,created_at,updated_at,workspace_id'),
        ('workspaces', 'phase,observed_generation,message,namespace,storage_gi_observed,storage_phase,storage_message',
         'id,customer_id,name,environment,desired,generation,zetta_service_id,created_at,updated_at,store_moved_at,storage_gi')
    LOOP
        EXECUTE format('GRANT SELECT, DELETE ON public.%I TO lake_runtime', t);
        EXECUTE format('GRANT INSERT (%s), UPDATE (%s) ON public.%I TO lake_runtime', desired_cols, desired_cols, t);
        EXECUTE format('GRANT UPDATE (%s, updated_at) ON public.%I TO lake_operator', status_cols, t);
    END LOOP;
END $$;
-- Lake's rotate_tokens request clears apps.token_hashes_json today (companion work: operator#813).
GRANT UPDATE (token_hashes_json) ON public.apps TO lake_runtime, lake_operator;

-- No access to users or sessions: only the email needed by the tunnel Access policy.
CREATE OR REPLACE VIEW public.lake_operator_tunnel_users AS SELECT id, email FROM public.users;
REVOKE ALL ON public.lake_operator_tunnel_users FROM PUBLIC, lake_runtime, lake_operator;
GRANT SELECT ON public.lake_operator_tunnel_users TO lake_operator;
GRANT SELECT ON public.workspaces, public.services, public.stores, public.store_mounts,
    public.apps, public.builds, public.agent_launches, public.agent_cron_schedules,
    public.methodology_runs, public.methodology_deployments, public.network_connections,
    public.network_egress_rules, public.network_tunnel_grants, public.network_peers,
    public.volume_snapshot_policies, public.cloud_orders TO lake_operator;
GRANT SELECT, INSERT, UPDATE ON public.service_status, public.store_status,
    public.agent_launch_status, public.agent_cron_schedule_status, public.agent_cron_runs,
    public.methodology_run_status, public.methodology_deployment_status,
    public.network_connection_status, public.network_egress_rule_status,
    public.network_tunnel_grant_status, public.network_peer_status,
    public.volume_snapshot_status TO lake_operator;
GRANT SELECT, INSERT, UPDATE, DELETE ON public.workload_resources, public.app_transfer TO lake_operator;
GRANT UPDATE (state, host, failure_reason, claimed_by, claimed_at) ON public.cloud_orders TO lake_operator;
GRANT SELECT (order_id), INSERT (order_id, at, from_state, to_state, actor, detail) ON public.cloud_order_events TO lake_operator;
-- cloud_order_events' bigserial id uses the owner's sequence, without sequence administration.
GRANT USAGE ON SEQUENCE public.cloud_order_events_id_seq TO lake_runtime, lake_operator;

GRANT USAGE ON SEQUENCE public.audit_events_sequence_seq, public.billing_invoices_number_seq TO lake_runtime;
