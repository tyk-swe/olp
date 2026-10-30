ALTER TABLE olp.code_attempts
    ADD COLUMN nonexecution boolean NOT NULL DEFAULT false,
    ADD COLUMN initial_recovery boolean NOT NULL DEFAULT false,
    ADD COLUMN recovery_from uuid UNIQUE REFERENCES olp.code_attempts;

CREATE FUNCTION olp.code_initial_recovery_safe(root uuid) RETURNS boolean LANGUAGE sql STABLE AS $$
    SELECT EXISTS(SELECT 1 FROM olp.code_bindings b WHERE b.id=root AND b.root_id=b.id AND b.parent_id IS NULL AND b.retired_at IS NULL)
      AND (SELECT count(*) FROM olp.code_bindings WHERE root_id=root)=1
      AND (SELECT count(*) FROM olp.code_pins WHERE root_id=root)=1
      AND NOT EXISTS(SELECT 1 FROM olp.code_references WHERE binding_id=root)
      AND EXISTS(SELECT 1 FROM olp.code_attempts WHERE binding_id=root)
      AND NOT EXISTS(SELECT 1 FROM olp.code_attempts WHERE binding_id=root
          AND (NOT initial_recovery OR NOT nonexecution OR state<>'settled' OR reported_tokens IS DISTINCT FROM 0))
$$;

CREATE OR REPLACE FUNCTION olp.preserve_code_binding() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    IF TG_OP='UPDATE' AND olp.code_initial_recovery_safe(OLD.id)
       AND to_jsonb(NEW)-ARRAY['account_id','principal'] IS NOT DISTINCT FROM to_jsonb(OLD)-ARRAY['account_id','principal'] THEN
        RETURN NEW;
    END IF;
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'code binding retirement is durable' USING ERRCODE='23514';
    END IF;
    IF OLD.retired_at IS NOT NULL OR NEW.retired_at IS NULL OR
       to_jsonb(NEW)-'retired_at' IS DISTINCT FROM to_jsonb(OLD)-'retired_at' THEN
        RAISE EXCEPTION 'code binding changes only by retirement or proven initial nonexecution' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION olp.preserve_code_pin() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    IF TG_OP='UPDATE' AND olp.code_initial_recovery_safe(OLD.root_id)
       AND to_jsonb(NEW)-ARRAY['account_id','principal','adapter'] IS NOT DISTINCT FROM to_jsonb(OLD)-ARRAY['account_id','principal','adapter'] THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'code pins are durable after execution' USING ERRCODE='23514';
END $$;
