ALTER TABLE olp.code_attempts ADD COLUMN requested_model text CHECK (octet_length(requested_model) BETWEEN 1 AND 200);

CREATE OR REPLACE FUNCTION olp.preserve_code_pin() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    IF TG_OP='UPDATE' AND olp.code_initial_recovery_safe(OLD.root_id)
       AND to_jsonb(NEW)-ARRAY['account_id','principal','adapter','target_model'] IS NOT DISTINCT FROM to_jsonb(OLD)-ARRAY['account_id','principal','adapter','target_model']
       AND (NEW.target_model IS NOT DISTINCT FROM OLD.target_model OR NEW.target_model=coalesce(OLD.target_model,OLD.model) OR EXISTS(
           SELECT 1 FROM olp.code_bindings b JOIN olp.code_routes r ON r.id=b.route_id JOIN olp.code_route_revisions v ON v.id=r.latest_revision_id
           WHERE b.id=OLD.root_id AND v.document->>'model_rewriting'='true'
             AND v.document->'model_pools'->OLD.model ? NEW.target_model AND v.document->'models' ? NEW.target_model)) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'code pins are durable after execution' USING ERRCODE='23514';
END $$;
