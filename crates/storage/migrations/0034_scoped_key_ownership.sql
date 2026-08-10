-- Introduce installation-local teams and projects without changing the
-- installation itself as the organization boundary. Service accounts are
-- deliberately project-scoped; this keeps their credential authority concrete.
CREATE TYPE scoped_membership_role AS ENUM ('admin', 'member');

CREATE TABLE teams (
    id uuid PRIMARY KEY,
    name text NOT NULL UNIQUE CHECK (length(name) BETWEEN 1 AND 120),
    active boolean NOT NULL DEFAULT true,
    etag uuid NOT NULL,
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE projects (
    id uuid PRIMARY KEY,
    team_id uuid NOT NULL REFERENCES teams(id) ON DELETE RESTRICT,
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
    active boolean NOT NULL DEFAULT true,
    etag uuid NOT NULL,
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, team_id),
    UNIQUE (team_id, name)
);

CREATE TABLE service_accounts (
    id uuid PRIMARY KEY,
    team_id uuid NOT NULL,
    project_id uuid NOT NULL,
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
    active boolean NOT NULL DEFAULT true,
    etag uuid NOT NULL,
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, team_id, project_id),
    UNIQUE (project_id, name),
    FOREIGN KEY (project_id, team_id)
        REFERENCES projects(id, team_id) ON DELETE RESTRICT
);

CREATE TABLE team_memberships (
    team_id uuid NOT NULL REFERENCES teams(id) ON DELETE RESTRICT,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    role scoped_membership_role NOT NULL,
    etag uuid NOT NULL,
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id, user_id)
);

CREATE TABLE project_memberships (
    project_id uuid NOT NULL,
    team_id uuid NOT NULL,
    user_id uuid NOT NULL,
    role scoped_membership_role NOT NULL,
    etag uuid NOT NULL,
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, user_id),
    FOREIGN KEY (project_id, team_id)
        REFERENCES projects(id, team_id) ON DELETE RESTRICT,
    FOREIGN KEY (team_id, user_id)
        REFERENCES team_memberships(team_id, user_id) ON DELETE RESTRICT
);

CREATE INDEX projects_team_idx ON projects(team_id, id);
CREATE INDEX service_accounts_project_idx ON service_accounts(project_id, id);
CREATE INDEX team_memberships_user_idx ON team_memberships(user_id, team_id);
CREATE INDEX project_memberships_user_idx ON project_memberships(user_id, project_id);

ALTER TABLE api_keys
    ADD COLUMN owner_user_id uuid REFERENCES users(id) ON DELETE RESTRICT,
    ADD COLUMN owner_service_account_id uuid REFERENCES service_accounts(id) ON DELETE RESTRICT,
    ADD COLUMN team_id uuid REFERENCES teams(id) ON DELETE RESTRICT,
    ADD COLUMN project_id uuid,
    ADD CONSTRAINT api_keys_single_owner_check CHECK (
        (owner_user_id IS NOT NULL)::integer +
        (owner_service_account_id IS NOT NULL)::integer = 1
    ) NOT VALID,
    ADD CONSTRAINT api_keys_project_requires_team_check
        CHECK (project_id IS NULL OR team_id IS NOT NULL) NOT VALID,
    ADD CONSTRAINT api_keys_service_account_scope_check CHECK (
        owner_service_account_id IS NULL OR
        (team_id IS NOT NULL AND project_id IS NOT NULL)
    ) NOT VALID,
    ADD CONSTRAINT api_keys_project_team_fk FOREIGN KEY (project_id, team_id)
        REFERENCES projects(id, team_id) ON DELETE RESTRICT,
    ADD CONSTRAINT api_keys_service_account_scope_fk
        FOREIGN KEY (owner_service_account_id, team_id, project_id)
        REFERENCES service_accounts(id, team_id, project_id) ON DELETE RESTRICT;

-- Every existing key was implicitly owned by its creator. Preserve that
-- behavior explicitly and retire keys belonging to users already disabled.
UPDATE api_keys SET owner_user_id = created_by;
UPDATE api_keys key
   SET revoked_at = COALESCE(key.revoked_at, now()), etag = gen_random_uuid()
  FROM users owner
 WHERE owner.id = key.owner_user_id AND NOT owner.active AND key.revoked_at IS NULL;

ALTER TABLE api_keys VALIDATE CONSTRAINT api_keys_single_owner_check;
ALTER TABLE api_keys VALIDATE CONSTRAINT api_keys_project_requires_team_check;
ALTER TABLE api_keys VALIDATE CONSTRAINT api_keys_service_account_scope_check;

CREATE FUNCTION enforce_api_key_owner_scope() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF NEW.created_by IS DISTINCT FROM OLD.created_by OR
           NEW.owner_user_id IS DISTINCT FROM OLD.owner_user_id OR
           NEW.owner_service_account_id IS DISTINCT FROM OLD.owner_service_account_id OR
           NEW.team_id IS DISTINCT FROM OLD.team_id OR
           NEW.project_id IS DISTINCT FROM OLD.project_id THEN
            RAISE EXCEPTION 'API key creator, owner, and scope are immutable'
                USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;

    -- Binaries from before this migration only know created_by. Preserve their
    -- insert behavior during a rolling upgrade while storing an explicit owner.
    IF NEW.owner_user_id IS NULL AND NEW.owner_service_account_id IS NULL THEN
        NEW.owner_user_id := NEW.created_by;
    END IF;

    IF NEW.owner_user_id IS NOT NULL THEN
        IF NOT EXISTS (SELECT 1 FROM users WHERE id = NEW.owner_user_id AND active) THEN
            RAISE EXCEPTION 'API key owner user must be active'
                USING ERRCODE = 'check_violation';
        END IF;
        IF NEW.team_id IS NOT NULL AND NOT EXISTS (
            SELECT 1 FROM teams team
            JOIN team_memberships membership ON membership.team_id = team.id
             AND membership.user_id = NEW.owner_user_id
            WHERE team.id = NEW.team_id AND team.active
        ) THEN
            RAISE EXCEPTION 'API key owner user must belong to the active team'
                USING ERRCODE = 'check_violation';
        END IF;
        IF NEW.project_id IS NOT NULL AND NOT EXISTS (
            SELECT 1 FROM projects project
            JOIN project_memberships membership ON membership.project_id = project.id
             AND membership.user_id = NEW.owner_user_id
            WHERE project.id = NEW.project_id AND project.team_id = NEW.team_id
              AND project.active
        ) THEN
            RAISE EXCEPTION 'API key owner user must belong to the active project'
                USING ERRCODE = 'check_violation';
        END IF;
    ELSE
        IF NOT EXISTS (
            SELECT 1 FROM service_accounts account
            JOIN projects project ON project.id = account.project_id
            JOIN teams team ON team.id = account.team_id
            WHERE account.id = NEW.owner_service_account_id
              AND account.team_id = NEW.team_id
              AND account.project_id = NEW.project_id
              AND account.active AND project.active AND team.active
        ) THEN
            RAISE EXCEPTION 'API key owner service account and scope must be active'
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER api_keys_owner_scope_guard
BEFORE INSERT OR UPDATE ON api_keys
FOR EACH ROW EXECUTE FUNCTION enforce_api_key_owner_scope();

-- Attribution is captured as immutable identifiers. The trigger also keeps
-- rolling-upgrade writers safe: omitted fields are filled at insert time, not
-- reconstructed by readers from later membership state.
ALTER TABLE requests
    ADD COLUMN owner_user_id uuid REFERENCES users(id) ON DELETE RESTRICT,
    ADD COLUMN service_account_id uuid REFERENCES service_accounts(id) ON DELETE RESTRICT,
    ADD COLUMN team_id uuid REFERENCES teams(id) ON DELETE RESTRICT,
    ADD COLUMN project_id uuid REFERENCES projects(id) ON DELETE RESTRICT;
ALTER TABLE attempts
    ADD COLUMN api_key_id uuid REFERENCES api_keys(id) ON DELETE RESTRICT,
    ADD COLUMN owner_user_id uuid REFERENCES users(id) ON DELETE RESTRICT,
    ADD COLUMN service_account_id uuid REFERENCES service_accounts(id) ON DELETE RESTRICT,
    ADD COLUMN team_id uuid REFERENCES teams(id) ON DELETE RESTRICT,
    ADD COLUMN project_id uuid REFERENCES projects(id) ON DELETE RESTRICT;
ALTER TABLE usage_facts
    ADD COLUMN owner_user_id uuid REFERENCES users(id) ON DELETE RESTRICT,
    ADD COLUMN service_account_id uuid REFERENCES service_accounts(id) ON DELETE RESTRICT,
    ADD COLUMN team_id uuid REFERENCES teams(id) ON DELETE RESTRICT,
    ADD COLUMN project_id uuid REFERENCES projects(id) ON DELETE RESTRICT;
ALTER TABLE attempt_usage_facts
    ADD COLUMN owner_user_id uuid REFERENCES users(id) ON DELETE RESTRICT,
    ADD COLUMN service_account_id uuid REFERENCES service_accounts(id) ON DELETE RESTRICT,
    ADD COLUMN team_id uuid REFERENCES teams(id) ON DELETE RESTRICT,
    ADD COLUMN project_id uuid REFERENCES projects(id) ON DELETE RESTRICT;
ALTER TABLE usage_hourly
    ADD COLUMN owner_user_id uuid REFERENCES users(id) ON DELETE RESTRICT,
    ADD COLUMN service_account_id uuid REFERENCES service_accounts(id) ON DELETE RESTRICT,
    ADD COLUMN team_id uuid REFERENCES teams(id) ON DELETE RESTRICT,
    ADD COLUMN project_id uuid REFERENCES projects(id) ON DELETE RESTRICT;
ALTER TABLE attempt_usage_hourly
    ADD COLUMN owner_user_id uuid REFERENCES users(id) ON DELETE RESTRICT,
    ADD COLUMN service_account_id uuid REFERENCES service_accounts(id) ON DELETE RESTRICT,
    ADD COLUMN team_id uuid REFERENCES teams(id) ON DELETE RESTRICT,
    ADD COLUMN project_id uuid REFERENCES projects(id) ON DELETE RESTRICT;

UPDATE requests request
   SET owner_user_id = key.owner_user_id,
       service_account_id = key.owner_service_account_id,
       team_id = key.team_id,
       project_id = key.project_id
  FROM api_keys key WHERE key.id = request.api_key_id;
UPDATE attempts attempt
   SET api_key_id = request.api_key_id,
       owner_user_id = request.owner_user_id,
       service_account_id = request.service_account_id,
       team_id = request.team_id,
       project_id = request.project_id
  FROM requests request
 WHERE request.id = attempt.request_id
   AND request.started_at = attempt.request_started_at;
UPDATE usage_facts fact
   SET owner_user_id = key.owner_user_id,
       service_account_id = key.owner_service_account_id,
       team_id = key.team_id,
       project_id = key.project_id
  FROM api_keys key WHERE key.id = fact.api_key_id;
UPDATE attempt_usage_facts fact
   SET owner_user_id = key.owner_user_id,
       service_account_id = key.owner_service_account_id,
       team_id = key.team_id,
       project_id = key.project_id
  FROM api_keys key WHERE key.id = fact.api_key_id;
SELECT set_config('olp.usage_rollup_writer', 'additive-v2', true);
SELECT set_config('olp.attempt_usage_hourly_mirror', 'off', true);
UPDATE usage_hourly hourly
   SET owner_user_id = key.owner_user_id,
       service_account_id = key.owner_service_account_id,
       team_id = key.team_id,
       project_id = key.project_id
  FROM api_keys key WHERE key.id = hourly.api_key_id;
UPDATE attempt_usage_hourly hourly
   SET owner_user_id = key.owner_user_id,
       service_account_id = key.owner_service_account_id,
       team_id = key.team_id,
       project_id = key.project_id
  FROM api_keys key WHERE key.id = hourly.api_key_id;

ALTER TABLE requests
    ADD CONSTRAINT requests_owner_attribution_check CHECK (
        (owner_user_id IS NOT NULL)::integer +
        (service_account_id IS NOT NULL)::integer = 1
    ),
    ADD CONSTRAINT requests_project_requires_team_check
        CHECK (project_id IS NULL OR team_id IS NOT NULL);
ALTER TABLE attempts
    ALTER COLUMN api_key_id SET NOT NULL,
    ADD CONSTRAINT attempts_owner_attribution_check CHECK (
        (owner_user_id IS NOT NULL)::integer +
        (service_account_id IS NOT NULL)::integer = 1
    ),
    ADD CONSTRAINT attempts_project_requires_team_check
        CHECK (project_id IS NULL OR team_id IS NOT NULL);
ALTER TABLE usage_facts
    ADD CONSTRAINT usage_facts_owner_attribution_check CHECK (
        (owner_user_id IS NOT NULL)::integer +
        (service_account_id IS NOT NULL)::integer = 1
    );
ALTER TABLE attempt_usage_facts
    ADD CONSTRAINT attempt_usage_facts_owner_attribution_check CHECK (
        (owner_user_id IS NOT NULL)::integer +
        (service_account_id IS NOT NULL)::integer = 1
    );
ALTER TABLE usage_hourly
    ADD CONSTRAINT usage_hourly_owner_attribution_check CHECK (
        (api_key_id IS NULL AND owner_user_id IS NULL AND service_account_id IS NULL AND
         team_id IS NULL AND project_id IS NULL) OR
        (api_key_id IS NOT NULL AND
         ((owner_user_id IS NOT NULL)::integer +
          (service_account_id IS NOT NULL)::integer = 1))
    );
ALTER TABLE attempt_usage_hourly
    ADD CONSTRAINT attempt_usage_hourly_owner_attribution_check CHECK (
        (api_key_id IS NULL AND owner_user_id IS NULL AND service_account_id IS NULL AND
         team_id IS NULL AND project_id IS NULL) OR
        (api_key_id IS NOT NULL AND
         ((owner_user_id IS NOT NULL)::integer +
          (service_account_id IS NOT NULL)::integer = 1))
    );

CREATE FUNCTION enforce_immutable_api_key_attribution() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    expected_api_key_id uuid;
    expected_owner_user_id uuid;
    expected_service_account_id uuid;
    expected_team_id uuid;
    expected_project_id uuid;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF NEW.api_key_id IS DISTINCT FROM OLD.api_key_id OR
           NEW.owner_user_id IS DISTINCT FROM OLD.owner_user_id OR
           NEW.service_account_id IS DISTINCT FROM OLD.service_account_id OR
           NEW.team_id IS DISTINCT FROM OLD.team_id OR
           NEW.project_id IS DISTINCT FROM OLD.project_id THEN
            RAISE EXCEPTION 'API key attribution is immutable'
                USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;

    IF TG_TABLE_NAME = 'attempts' AND NEW.api_key_id IS NULL THEN
        SELECT request.api_key_id, request.owner_user_id, request.service_account_id,
               request.team_id, request.project_id
          INTO expected_api_key_id, expected_owner_user_id, expected_service_account_id,
               expected_team_id, expected_project_id
          FROM requests request
         WHERE request.id = NEW.request_id AND request.started_at = NEW.request_started_at;
        NEW.api_key_id := expected_api_key_id;
    ELSIF NEW.api_key_id IS NOT NULL THEN
        SELECT key.id, key.owner_user_id, key.owner_service_account_id,
               key.team_id, key.project_id
          INTO expected_api_key_id, expected_owner_user_id, expected_service_account_id,
               expected_team_id, expected_project_id
          FROM api_keys key WHERE key.id = NEW.api_key_id;
    END IF;

    IF NEW.api_key_id IS NULL THEN
        IF NEW.owner_user_id IS NOT NULL OR NEW.service_account_id IS NOT NULL OR
           NEW.team_id IS NOT NULL OR NEW.project_id IS NOT NULL THEN
            RAISE EXCEPTION 'unkeyed attribution must not identify an owner or scope'
                USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF expected_api_key_id IS NULL THEN
        RAISE EXCEPTION 'API key attribution references an unknown key'
            USING ERRCODE = 'foreign_key_violation';
    END IF;

    IF NEW.owner_user_id IS NULL AND NEW.service_account_id IS NULL THEN
        NEW.owner_user_id := expected_owner_user_id;
        NEW.service_account_id := expected_service_account_id;
        NEW.team_id := expected_team_id;
        NEW.project_id := expected_project_id;
    ELSIF NEW.owner_user_id IS DISTINCT FROM expected_owner_user_id OR
          NEW.service_account_id IS DISTINCT FROM expected_service_account_id OR
          NEW.team_id IS DISTINCT FROM expected_team_id OR
          NEW.project_id IS DISTINCT FROM expected_project_id THEN
        RAISE EXCEPTION 'API key attribution does not match the immutable key scope'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER requests_api_key_attribution_guard
BEFORE INSERT OR UPDATE OF api_key_id, owner_user_id, service_account_id, team_id, project_id
ON requests FOR EACH ROW EXECUTE FUNCTION enforce_immutable_api_key_attribution();
CREATE TRIGGER attempts_api_key_attribution_guard
BEFORE INSERT OR UPDATE OF api_key_id, owner_user_id, service_account_id, team_id, project_id
ON attempts FOR EACH ROW EXECUTE FUNCTION enforce_immutable_api_key_attribution();
CREATE TRIGGER usage_facts_api_key_attribution_guard
BEFORE INSERT OR UPDATE OF api_key_id, owner_user_id, service_account_id, team_id, project_id
ON usage_facts FOR EACH ROW EXECUTE FUNCTION enforce_immutable_api_key_attribution();
CREATE TRIGGER attempt_usage_facts_api_key_attribution_guard
BEFORE INSERT OR UPDATE OF api_key_id, owner_user_id, service_account_id, team_id, project_id
ON attempt_usage_facts FOR EACH ROW EXECUTE FUNCTION enforce_immutable_api_key_attribution();
CREATE TRIGGER usage_hourly_api_key_attribution_guard
BEFORE INSERT OR UPDATE OF api_key_id, owner_user_id, service_account_id, team_id, project_id
ON usage_hourly FOR EACH ROW EXECUTE FUNCTION enforce_immutable_api_key_attribution();
CREATE TRIGGER attempt_usage_hourly_api_key_attribution_guard
BEFORE INSERT OR UPDATE OF api_key_id, owner_user_id, service_account_id, team_id, project_id
ON attempt_usage_hourly FOR EACH ROW EXECUTE FUNCTION enforce_immutable_api_key_attribution();

CREATE INDEX api_keys_owner_user_idx ON api_keys(owner_user_id, id);
CREATE INDEX api_keys_owner_service_account_idx ON api_keys(owner_service_account_id, id);
CREATE INDEX api_keys_team_project_idx ON api_keys(team_id, project_id, id);
CREATE INDEX requests_scope_idx ON requests(team_id, project_id, started_at DESC);
CREATE INDEX attempt_usage_facts_scope_idx
    ON attempt_usage_facts(team_id, project_id, observed_at DESC);
