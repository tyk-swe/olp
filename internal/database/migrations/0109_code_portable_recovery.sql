ALTER TABLE olp.code_bindings ADD COLUMN socket_bound boolean NOT NULL DEFAULT true;

CREATE OR REPLACE FUNCTION olp.code_initial_recovery_safe(root uuid) RETURNS boolean LANGUAGE sql STABLE AS $$
    SELECT EXISTS(SELECT 1 FROM olp.code_bindings b WHERE b.id=root AND b.root_id=b.id AND b.parent_id IS NULL AND b.retired_at IS NULL AND NOT b.socket_bound)
      AND (SELECT count(*) FROM olp.code_bindings WHERE root_id=root)=1
      AND (SELECT count(*) FROM olp.code_pins WHERE root_id=root)=1
      AND EXISTS(SELECT 1 FROM olp.code_attempts a WHERE a.binding_id=root AND a.initial_recovery AND a.nonexecution AND a.state='settled' AND a.reported_tokens=0)
      AND NOT EXISTS(SELECT 1 FROM olp.code_attempts WHERE binding_id=root
          AND (state NOT IN ('settled','aborted') OR state='settled' AND reported_tokens IS NULL))
$$;

CREATE OR REPLACE FUNCTION olp.preserve_code_binding() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    IF TG_OP='UPDATE' AND olp.code_initial_recovery_safe(OLD.id)
       AND to_jsonb(NEW)-ARRAY['account_id','principal'] IS NOT DISTINCT FROM to_jsonb(OLD)-ARRAY['account_id','principal'] THEN
        RETURN NEW;
    END IF;
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'code binding retirement is durable' USING ERRCODE='23514';
    END IF;
    IF to_jsonb(NEW)-ARRAY['retired_at','socket_bound'] IS DISTINCT FROM to_jsonb(OLD)-ARRAY['retired_at','socket_bound']
       OR OLD.socket_bound AND NOT NEW.socket_bound
       OR OLD.retired_at IS DISTINCT FROM NEW.retired_at AND (OLD.retired_at IS NOT NULL OR NEW.retired_at IS NULL) THEN
        RAISE EXCEPTION 'code binding changes only by retirement, socket ownership or proven portable nonexecution' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END $$;
