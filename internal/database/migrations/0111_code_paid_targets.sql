ALTER TABLE olp.code_route_revisions ADD COLUMN paid_connections jsonb NOT NULL DEFAULT '{}';

CREATE TABLE olp.code_paid_bindings (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES olp.projects,
    route_id uuid NOT NULL REFERENCES olp.code_routes,
    api_key_id uuid NOT NULL REFERENCES olp.api_keys,
    conversation text NOT NULL,
    parent_id uuid REFERENCES olp.code_paid_bindings,
    root_id uuid NOT NULL REFERENCES olp.code_paid_bindings DEFERRABLE INITIALLY DEFERRED,
    model text NOT NULL,
    selected_model text NOT NULL,
    provider_id uuid NOT NULL REFERENCES olp.providers,
    slot_id uuid NOT NULL REFERENCES olp.provider_slots,
    credential_id uuid NOT NULL REFERENCES olp.provider_credentials,
    created_at timestamptz NOT NULL DEFAULT now(),
    retired_at timestamptz,
    UNIQUE(route_id,api_key_id,conversation,model)
);

ALTER TABLE olp.code_attempts
    ALTER COLUMN binding_id DROP NOT NULL,
    ALTER COLUMN account_id DROP NOT NULL,
    ADD COLUMN paid_binding_id uuid REFERENCES olp.code_paid_bindings,
    ADD COLUMN paid_provider_id uuid REFERENCES olp.providers,
    ADD COLUMN paid_slot_id uuid REFERENCES olp.provider_slots,
    ADD COLUMN paid_cost_owners uuid[] NOT NULL DEFAULT '{}',
    ADD CONSTRAINT code_attempt_target CHECK (
        (binding_id IS NOT NULL AND account_id IS NOT NULL AND paid_binding_id IS NULL AND paid_provider_id IS NULL AND paid_slot_id IS NULL)
        OR (binding_id IS NULL AND account_id IS NULL AND paid_binding_id IS NOT NULL AND paid_provider_id IS NOT NULL AND paid_slot_id IS NOT NULL)
    );

CREATE TABLE olp.code_paid_references (
    route_id uuid NOT NULL REFERENCES olp.code_routes,
    api_key_id uuid NOT NULL REFERENCES olp.api_keys,
    external_id text NOT NULL,
    binding_id uuid NOT NULL REFERENCES olp.code_paid_bindings,
    ambiguous boolean NOT NULL DEFAULT false,
    PRIMARY KEY(route_id,api_key_id,external_id)
);

CREATE INDEX code_paid_pending_owners ON olp.code_attempts USING gin(paid_cost_owners)
    WHERE paid_binding_id IS NOT NULL AND state NOT IN ('prepared','aborted');

ALTER FUNCTION olp.has_pending_resource_usage(uuid) RENAME TO has_pending_resource_usage_before_code_paid;
CREATE FUNCTION olp.has_pending_resource_usage(owner uuid) RETURNS boolean LANGUAGE sql STABLE AS $$
    SELECT olp.has_pending_resource_usage_before_code_paid(owner)
        OR EXISTS(SELECT 1 FROM olp.code_attempts a
            WHERE paid_binding_id IS NOT NULL AND state NOT IN ('prepared','aborted')
                AND paid_cost_owners @> ARRAY[owner]
                AND NOT EXISTS(SELECT 1 FROM olp.attempt_usage_facts f WHERE f.request_id=a.id
                    AND f.usage_complete AND f.estimated_cost IS NOT NULL
                    AND (owner IN (a.paid_provider_id,a.paid_slot_id) OR (f.charge_status<>'billing_uncertain' AND f.charge IS NOT NULL))))
$$;

CREATE FUNCTION olp.observe_code_reference(target_attempt uuid,reference_id text)
RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(a.route_id::text||':'||a.api_key_id::text,0))
    FROM olp.code_attempts a WHERE a.id=target_attempt;

    INSERT INTO olp.code_paid_references(route_id,api_key_id,external_id,binding_id)
    SELECT a.route_id,a.api_key_id,reference_id,a.paid_binding_id
    FROM olp.code_attempts a
    WHERE a.id=target_attempt AND a.paid_binding_id IS NOT NULL AND a.state<>'prepared'
    ON CONFLICT(route_id,api_key_id,external_id) DO UPDATE SET
        ambiguous=olp.code_paid_references.ambiguous OR
        (SELECT root_id FROM olp.code_paid_bindings WHERE id=olp.code_paid_references.binding_id)<>
        (SELECT root_id FROM olp.code_paid_bindings WHERE id=EXCLUDED.binding_id);

    INSERT INTO olp.code_references(route_id,api_key_id,external_id,binding_id,account_id)
    SELECT a.route_id,a.api_key_id,reference_id,a.binding_id,a.account_id
    FROM olp.code_attempts a
    WHERE a.id=target_attempt AND a.state<>'prepared' AND a.paid_binding_id IS NULL
    ON CONFLICT(route_id,api_key_id,external_id) DO UPDATE SET
        ambiguous=olp.code_references.ambiguous OR
        olp.code_references.account_id IS DISTINCT FROM EXCLUDED.account_id OR
        (SELECT root_id FROM olp.code_bindings WHERE id=olp.code_references.binding_id)<>
        (SELECT root_id FROM olp.code_bindings WHERE id=EXCLUDED.binding_id);

    UPDATE olp.code_paid_references p SET ambiguous=true
    FROM olp.code_attempts a
    WHERE a.id=target_attempt AND p.route_id=a.route_id AND p.api_key_id=a.api_key_id
        AND p.external_id=reference_id AND EXISTS(
            SELECT 1 FROM olp.code_references r WHERE r.route_id=p.route_id
                AND r.api_key_id=p.api_key_id AND r.external_id=p.external_id);

    UPDATE olp.code_references r SET ambiguous=true
    FROM olp.code_attempts a
    WHERE a.id=target_attempt AND r.route_id=a.route_id AND r.api_key_id=a.api_key_id
        AND r.external_id=reference_id AND EXISTS(
            SELECT 1 FROM olp.code_paid_references p WHERE p.route_id=r.route_id
                AND p.api_key_id=r.api_key_id AND p.external_id=r.external_id);
END
$$;
