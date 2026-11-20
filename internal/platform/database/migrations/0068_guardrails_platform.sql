ALTER TABLE olp.guardrails DROP CONSTRAINT guardrails_type_check;
ALTER TABLE olp.guardrails ADD CONSTRAINT guardrails_type_check CHECK (type IN
 ('builtin.regex','builtin.pii','builtin.secrets','builtin.keywords','builtin.schema','builtin.tools',
  'webhook','bedrock','azure.content_safety','azure.prompt_shields','model_armor','lakera','presidio','model','plugin'));
ALTER TABLE olp.guardrails ALTER COLUMN project_id DROP NOT NULL;
ALTER TABLE olp.guardrails ADD COLUMN active_revision_id uuid;
ALTER TABLE olp.guardrails ADD COLUMN managed_route_revision_id uuid REFERENCES olp.route_revisions(id);
ALTER TABLE olp.guardrails ADD CONSTRAINT guardrail_active_revision_owner
 FOREIGN KEY(active_revision_id,id) REFERENCES olp.guardrail_revisions(id,guardrail_id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE olp.guardrail_revisions ADD COLUMN configuration jsonb NOT NULL DEFAULT '{}';
ALTER TABLE olp.guardrail_revisions ADD COLUMN timeout_ms integer NOT NULL DEFAULT 1000 CHECK(timeout_ms BETWEEN 1 AND 300000);
ALTER TABLE olp.guardrail_revisions ADD COLUMN on_error text NOT NULL DEFAULT 'fail_closed' CHECK(on_error IN ('fail_closed','fail_open'));
ALTER TABLE olp.guardrail_revisions DISABLE TRIGGER guardrail_revision_immutable;
UPDATE olp.guardrail_revisions SET configuration=jsonb_build_object('rules',
 COALESCE((SELECT jsonb_agg(jsonb_build_object('id',r->>'id','pattern',r->>'pattern',
 'replacement',COALESCE(r->'replacement','"[REDACTED]"'::jsonb)) ORDER BY n)
 FROM jsonb_array_elements(policy->'rules') WITH ORDINALITY AS x(r,n)),'[]'::jsonb));
ALTER TABLE olp.guardrail_revisions ENABLE TRIGGER guardrail_revision_immutable;
UPDATE olp.guardrails SET active_revision_id=latest_revision_id;

CREATE TABLE olp.guardrail_policies (
 id uuid PRIMARY KEY, project_id uuid REFERENCES olp.projects(id), name text NOT NULL CHECK(length(name) BETWEEN 1 AND 100),
 latest_revision_id uuid NOT NULL, active_revision_id uuid, etag uuid NOT NULL,
 retired_at timestamptz, created_by uuid NOT NULL REFERENCES olp.users(id), created_at timestamptz NOT NULL DEFAULT now(),
 managed_route_revision_id uuid REFERENCES olp.route_revisions(id)
);
CREATE UNIQUE INDEX guardrail_policies_project_name ON olp.guardrail_policies(project_id,lower(name)) WHERE retired_at IS NULL;
CREATE TABLE olp.guardrail_policy_revisions (
 id uuid PRIMARY KEY, policy_id uuid NOT NULL REFERENCES olp.guardrail_policies(id), revision bigint NOT NULL CHECK(revision>0),
 entries jsonb NOT NULL CHECK(jsonb_typeof(entries)='array' AND jsonb_array_length(entries)<=128),
 created_by uuid NOT NULL REFERENCES olp.users(id), created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(policy_id,revision), UNIQUE(id,policy_id)
);
ALTER TABLE olp.guardrail_policies ADD CONSTRAINT guardrail_policy_latest_owner FOREIGN KEY(latest_revision_id,id)
 REFERENCES olp.guardrail_policy_revisions(id,policy_id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE olp.guardrail_policies ADD CONSTRAINT guardrail_policy_active_owner FOREIGN KEY(active_revision_id,id)
 REFERENCES olp.guardrail_policy_revisions(id,policy_id) DEFERRABLE INITIALLY DEFERRED;
CREATE TRIGGER guardrail_policy_revision_immutable BEFORE UPDATE OR DELETE ON olp.guardrail_policy_revisions
 FOR EACH ROW EXECUTE FUNCTION olp.guardrail_revision_immutable();
CREATE TABLE olp.guardrail_revision_activations (
 revision_id uuid PRIMARY KEY REFERENCES olp.guardrail_revisions(id), activated_by uuid NOT NULL REFERENCES olp.users(id), activated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO olp.guardrail_revision_activations(revision_id,activated_by) SELECT id,created_by FROM olp.guardrail_revisions;
CREATE TABLE olp.guardrail_policy_revision_activations (
 revision_id uuid PRIMARY KEY REFERENCES olp.guardrail_policy_revisions(id), activated_by uuid NOT NULL REFERENCES olp.users(id), activated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE olp.guardrail_attachments (
 id uuid PRIMARY KEY, scope text NOT NULL CHECK(scope IN ('installation','organization','project','route','api-key')),
 scope_id uuid, policy_revision_id uuid NOT NULL REFERENCES olp.guardrail_policy_revisions(id),
 position integer NOT NULL CHECK(position>=0), etag uuid NOT NULL, created_by uuid NOT NULL REFERENCES olp.users(id),
 created_at timestamptz NOT NULL DEFAULT now(), CHECK((scope='installation')=(scope_id IS NULL))
);
CREATE UNIQUE INDEX guardrail_attachment_order ON olp.guardrail_attachments(scope,scope_id,position) NULLS NOT DISTINCT;
-- Route imports are staged alongside their route draft. Promotion copies each
-- attachment atomically into the published route and cannot remove another
-- policy occupying the same position.
CREATE TABLE olp.guardrail_draft_attachments (
 draft_id uuid NOT NULL REFERENCES olp.route_drafts(id),
 policy_revision_id uuid NOT NULL REFERENCES olp.guardrail_policy_revisions(id),
 position integer NOT NULL CHECK(position>=0), created_by uuid NOT NULL REFERENCES olp.users(id),
 PRIMARY KEY(draft_id,position)
);
CREATE TABLE olp.guardrail_credentials (
 id uuid PRIMARY KEY REFERENCES olp.secrets(id), project_id uuid NOT NULL REFERENCES olp.projects(id),
 name text NOT NULL, created_by uuid NOT NULL REFERENCES olp.users(id), created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(project_id,name)
);
ALTER TABLE olp.secrets DROP CONSTRAINT secrets_purpose_check;
ALTER TABLE olp.secrets ADD CONSTRAINT secrets_purpose_check CHECK (purpose IN
 ('provider_credential','provider_continuation','mfa_totp','mfa_webauthn','notification_secret','mutation_replay',
 'oidc_client','oidc_flow','media_job_source','provider_grant_refresh','grant_enrollment','saml_key','saml_flow',
 'sink_credential','mcp_credential','billing_credential','guardrail_credential'));
ALTER TABLE olp.requests ADD COLUMN guardrail_decisions jsonb NOT NULL DEFAULT '[]'
 CHECK(jsonb_typeof(guardrail_decisions)='array' AND octet_length(guardrail_decisions::text)<=262144);
ALTER TABLE olp.attempts ADD COLUMN guardrail_decisions jsonb NOT NULL DEFAULT '[]'
 CHECK(jsonb_typeof(guardrail_decisions)='array' AND octet_length(guardrail_decisions::text)<=262144);

-- Each historical route revision owns a separate immutable conversion. The
-- source rows and their old accounting evidence remain untouched.
CREATE FUNCTION olp.migrate_route_guardrails() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE project uuid; gid uuid; grev uuid; pid uuid; prev uuid; rule jsonb;
        entries jsonb := '[]'; ordinal integer := 0;
BEGIN
 IF NEW.content_policy IS NULL OR jsonb_array_length(COALESCE(NEW.content_policy->'rules','[]'))=0 THEN RETURN NEW; END IF;
 SELECT project_id INTO project FROM olp.routes WHERE id=NEW.route_id;
 pid:=gen_random_uuid(); prev:=gen_random_uuid();
 INSERT INTO olp.guardrail_policies(id,project_id,name,latest_revision_id,active_revision_id,etag,created_by,managed_route_revision_id)
 VALUES(pid,project,'route '||left(NEW.slug,32)||' '||NEW.id,prev,prev,gen_random_uuid(),NEW.activated_by,NEW.id);
 FOR rule IN SELECT value FROM jsonb_array_elements(NEW.content_policy->'rules') LOOP
  gid:=gen_random_uuid(); grev:=gen_random_uuid();
  INSERT INTO olp.guardrails(id,project_id,name,type,latest_revision_id,active_revision_id,etag,created_by,managed_route_revision_id)
  VALUES(gid,project,'route '||left(NEW.slug,32)||' '||NEW.id||' rule '||ordinal,'builtin.regex',grev,grev,gen_random_uuid(),NEW.activated_by,NEW.id);
  INSERT INTO olp.guardrail_revisions(id,guardrail_id,revision,policy,configuration,timeout_ms,created_by)
  VALUES(grev,gid,1,jsonb_build_object('rules',jsonb_build_array(rule)),jsonb_build_object('rules',jsonb_build_array(jsonb_build_object(
   'id',rule->>'id','pattern',rule->>'pattern','replacement',COALESCE(rule->'replacement','"[REDACTED]"'::jsonb)))),least(1000,NEW.overall_timeout_ms),NEW.activated_by);
  INSERT INTO olp.guardrail_revision_activations(revision_id,activated_by) VALUES(grev,NEW.activated_by);
  entries:=entries||jsonb_build_array(jsonb_build_object('guardrail_id',gid,'revision_id',grev,'phase',rule->>'phase','action',rule->>'action','mandatory',true));
  ordinal:=ordinal+1;
 END LOOP;
 INSERT INTO olp.guardrail_policy_revisions(id,policy_id,revision,entries,created_by) VALUES(prev,pid,1,entries,NEW.activated_by);
 INSERT INTO olp.guardrail_policy_revision_activations(revision_id,activated_by) VALUES(prev,NEW.activated_by);
 RETURN NEW;
END $$;
CREATE TRIGGER migrate_route_guardrails AFTER INSERT ON olp.route_revisions FOR EACH ROW EXECUTE FUNCTION olp.migrate_route_guardrails();
DO $$ DECLARE revision olp.route_revisions; BEGIN
 -- Invoke the same trigger for existing rows without updating immutable rows.
 -- A transient table lets the trigger use its normal NEW record contract.
 CREATE TEMP TABLE olp_guardrail_migration (LIKE olp.route_revisions INCLUDING DEFAULTS) ON COMMIT DROP;
 CREATE TRIGGER migrate_historical_guardrails AFTER INSERT ON olp_guardrail_migration FOR EACH ROW EXECUTE FUNCTION olp.migrate_route_guardrails();
 FOR revision IN SELECT * FROM olp.route_revisions WHERE content_policy IS NOT NULL LOOP
  INSERT INTO olp_guardrail_migration SELECT revision.*;
 END LOOP;
END $$;

-- Keep both historical regex evidence and the new bounded decision records in
-- the existing asynchronous export stream.
DO $$ BEGIN
 EXECUTE replace(pg_get_functiondef('olp.export_accounted_request()'::regprocedure),'v_request.policy_decisions','(v_request.policy_decisions || v_request.guardrail_decisions)');
 EXECUTE replace(pg_get_functiondef('olp.export_request_metadata()'::regprocedure),'NEW.policy_decisions','(NEW.policy_decisions || NEW.guardrail_decisions)');
END $$;
INSERT INTO olp.settings(key,value,etag,updated_by) SELECT 'guardrails.stream_window_bytes','65536',gen_random_uuid(),id FROM olp.users WHERE role='owner' ORDER BY id LIMIT 1;

CREATE TRIGGER guardrail_activation_immutable BEFORE UPDATE OR DELETE ON olp.guardrail_revision_activations FOR EACH ROW EXECUTE FUNCTION olp.guardrail_revision_immutable();
CREATE TRIGGER guardrail_policy_activation_immutable BEFORE UPDATE OR DELETE ON olp.guardrail_policy_revision_activations FOR EACH ROW EXECUTE FUNCTION olp.guardrail_revision_immutable();

ALTER TABLE olp.requests ADD COLUMN evaluator_guardrail_id uuid;
