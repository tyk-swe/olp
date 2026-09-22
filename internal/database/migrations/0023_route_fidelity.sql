ALTER TABLE olp_go.route_drafts ADD COLUMN fidelity jsonb
    CHECK (fidelity IS NULL OR (
        jsonb_typeof(fidelity) = 'object'
        AND jsonb_typeof(fidelity->'mode') = 'string'
        AND fidelity ? 'mode'
        AND fidelity->>'mode' IN ('legacy','strict','transformed')
        AND fidelity - 'mode' = '{}'::jsonb
    ));
ALTER TABLE olp_go.route_revisions ADD COLUMN fidelity jsonb
    CHECK (fidelity IS NULL OR (
        jsonb_typeof(fidelity) = 'object'
        AND jsonb_typeof(fidelity->'mode') = 'string'
        AND fidelity ? 'mode'
        AND fidelity->>'mode' IN ('legacy','strict','transformed')
        AND fidelity - 'mode' = '{}'::jsonb
    ));
