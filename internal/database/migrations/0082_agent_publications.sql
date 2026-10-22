CREATE TABLE olp.agent_publications (
 id uuid PRIMARY KEY,
 project_id uuid NOT NULL REFERENCES olp.projects,
 kind text NOT NULL CHECK(kind IN ('toolset','agent','prompt')),
 name text NOT NULL CHECK(length(name) BETWEEN 1 AND 100),
 draft jsonb NOT NULL CHECK(jsonb_typeof(draft)='object' AND octet_length(draft::text)<=1048576),
 latest_revision_id uuid,
 etag uuid NOT NULL,
 retired boolean NOT NULL DEFAULT false,
 created_by uuid NOT NULL REFERENCES olp.users,
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(id,project_id,kind)
);
CREATE UNIQUE INDEX agent_publications_name ON olp.agent_publications(project_id,kind,lower(name)) WHERE NOT retired;
CREATE TABLE olp.agent_credentials (
 id uuid PRIMARY KEY REFERENCES olp.secrets,
 project_id uuid NOT NULL REFERENCES olp.projects
);
CREATE TABLE olp.agent_publication_revisions (
 id uuid PRIMARY KEY,
 publication_id uuid NOT NULL,
 project_id uuid NOT NULL REFERENCES olp.projects,
 kind text NOT NULL CHECK(kind IN ('toolset','agent','prompt')),
 definition jsonb NOT NULL CHECK(jsonb_typeof(definition)='object' AND octet_length(definition::text)<=1048576),
 digest text NOT NULL CHECK(digest ~ '^[a-f0-9]{64}$'),
 created_by uuid NOT NULL REFERENCES olp.users,
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(publication_id,id),UNIQUE(publication_id,digest),
 FOREIGN KEY(publication_id,project_id,kind) REFERENCES olp.agent_publications(id,project_id,kind)
);
ALTER TABLE olp.agent_publications ADD CONSTRAINT agent_publications_latest FOREIGN KEY(id,latest_revision_id) REFERENCES olp.agent_publication_revisions(publication_id,id);
CREATE TRIGGER agent_publication_revisions_immutable BEFORE UPDATE OR DELETE ON olp.agent_publication_revisions FOR EACH ROW EXECUTE FUNCTION olp.reject_execution_evidence_mutation();
-- One live-authority predicate is shared by runtime refresh and approvals.
CREATE VIEW olp.live_agent_publication_revisions AS
 SELECT v.* FROM olp.agent_publication_revisions v JOIN olp.agent_publications p ON p.id=v.publication_id
 WHERE NOT p.retired AND (v.kind<>'toolset' OR NOT EXISTS(
 SELECT 1 FROM jsonb_array_elements(v.definition->'toolset'->'tools') t
 LEFT JOIN olp.mcp_servers s ON s.id=(t->>'server_id')::uuid
 WHERE s.id IS NULL OR NOT s.enabled OR s.retired_at IS NOT NULL
 OR s.project_id<>v.project_id OR s.latest_revision_id<>(t->>'revision_id')::uuid));

CREATE TABLE olp.prompt_labels (
 publication_id uuid NOT NULL REFERENCES olp.agent_publications,
 label text NOT NULL CHECK(label IN ('production','staging')),
 revision_id uuid NOT NULL,
 etag uuid NOT NULL,
 FOREIGN KEY(publication_id,revision_id) REFERENCES olp.agent_publication_revisions(publication_id,id),
 PRIMARY KEY(publication_id,label)
);
CREATE TABLE olp.agent_runs (
 id uuid PRIMARY KEY,
 api_key_id uuid NOT NULL REFERENCES olp.api_keys,
 project_id uuid NOT NULL REFERENCES olp.projects,
 revision_id uuid NOT NULL REFERENCES olp.agent_publication_revisions,
 principal_digest bytea NOT NULL CHECK(octet_length(principal_digest)=32),
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE olp.agent_steps (
 id uuid PRIMARY KEY,
 run_id uuid NOT NULL REFERENCES olp.agent_runs,
 dispatch_id uuid NOT NULL UNIQUE REFERENCES olp.execution_debits,
 ordinal integer NOT NULL CHECK(ordinal>=0),
 UNIQUE(run_id,ordinal), UNIQUE(run_id,id)
);
CREATE TRIGGER agent_runs_immutable BEFORE UPDATE ON olp.agent_runs FOR EACH ROW EXECUTE FUNCTION olp.reject_execution_evidence_mutation();
CREATE TRIGGER agent_steps_immutable BEFORE UPDATE ON olp.agent_steps FOR EACH ROW EXECUTE FUNCTION olp.reject_execution_evidence_mutation();
ALTER TABLE olp.execution_debits ADD CONSTRAINT execution_debits_run FOREIGN KEY(run_id) REFERENCES olp.agent_runs DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE olp.execution_debits ADD CONSTRAINT execution_debits_step FOREIGN KEY(run_id,step_id) REFERENCES olp.agent_steps(run_id,id) DEFERRABLE INITIALLY DEFERRED;
CREATE TABLE olp.agent_approvals (
 id uuid PRIMARY KEY,
 api_key_id uuid NOT NULL REFERENCES olp.api_keys,
 project_id uuid NOT NULL REFERENCES olp.projects,
 run_id uuid NOT NULL REFERENCES olp.agent_runs,
 binding_digest bytea NOT NULL CHECK(octet_length(binding_digest)=32),
 expires_at timestamptz NOT NULL,
 consumed_at timestamptz,
 dispatch_id uuid UNIQUE REFERENCES olp.execution_debits,
 CHECK((consumed_at IS NULL)=(dispatch_id IS NULL))
);
CREATE INDEX agent_approvals_expiry ON olp.agent_approvals(expires_at) WHERE consumed_at IS NULL;

CREATE FUNCTION olp.guard_agent_approval() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF ROW(NEW.id,NEW.api_key_id,NEW.project_id,NEW.run_id,NEW.binding_digest,NEW.expires_at)
 IS DISTINCT FROM ROW(OLD.id,OLD.api_key_id,OLD.project_id,OLD.run_id,OLD.binding_digest,OLD.expires_at)
 OR OLD.consumed_at IS NOT NULL THEN RAISE EXCEPTION 'approval evidence is immutable'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER agent_approvals_once BEFORE UPDATE ON olp.agent_approvals FOR EACH ROW EXECUTE FUNCTION olp.guard_agent_approval();
CREATE FUNCTION olp.check_approval_dispatch() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.consumed_at IS NOT NULL AND NOT EXISTS(
 SELECT 1 FROM olp.execution_debits d WHERE d.id=NEW.dispatch_id AND d.run_id=NEW.run_id
 AND d.api_key_id=NEW.api_key_id AND d.project_id=NEW.project_id
 AND d.state IN ('dispatched','outcome_unknown','settled','conflict','not_accepted'))
 THEN RAISE EXCEPTION 'approval must commit with its dispatch'; END IF;
 RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER agent_approval_dispatch AFTER INSERT OR UPDATE ON olp.agent_approvals DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION olp.check_approval_dispatch();

ALTER TABLE olp.worker_task_health DROP CONSTRAINT worker_task_health_task_check,
 ADD CONSTRAINT worker_task_health_task_check CHECK(task IN ('request_metadata_consumer','maintenance','cost_reconciliation','request_metadata_gateway_epoch_detection','media_reconciliation','notification_delivery','grant_refresh','health_probes','export_delivery','managed_export_delivery','focus_export','billing_import','batch_settlement','execution_debits'));
ALTER TABLE olp.secrets DROP CONSTRAINT secrets_purpose_check,
 ADD CONSTRAINT secrets_purpose_check CHECK(purpose IN ('saml_key','saml_flow','provider_credential','provider_continuation','mfa_totp','mfa_webauthn','notification_secret','mutation_replay','oidc_client','oidc_flow','media_job_source','provider_grant_refresh','grant_enrollment','sink_credential','mcp_credential','billing_credential','guardrail_credential','response_cache','provider_upload_session','agent_credential'));
