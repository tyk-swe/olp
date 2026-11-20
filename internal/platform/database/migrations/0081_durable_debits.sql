-- Provisional exposure is not usage. These rows are independent of resource,
-- request-history and ordinary lease retention, and contain no runtime bodies.
CREATE TABLE olp.execution_debits (
 id uuid PRIMARY KEY,
 api_key_id uuid NOT NULL REFERENCES olp.api_keys,
 project_id uuid REFERENCES olp.projects,
 run_id uuid,
 step_id uuid,
 kind text NOT NULL CHECK(kind IN ('api','model','tool','agent')),
 CHECK(project_id IS NOT NULL OR kind IN ('api','model')),
 affinity jsonb NOT NULL CHECK(jsonb_typeof(affinity)='object' AND octet_length(affinity::text)<=4096),
 intent_digest bytea NOT NULL CHECK(octet_length(intent_digest)=32),
 claim_id uuid NOT NULL,
 currency text NOT NULL CHECK(currency ~ '^[A-Z]{3}$'),
 cost_bound numeric(28,12) NOT NULL CHECK(cost_bound>=0),
 charge_bound numeric(28,12) NOT NULL CHECK(charge_bound>=0),
 observed_cost numeric(28,12) NOT NULL DEFAULT 0 CHECK(observed_cost>=0),
 observed_charge numeric(28,12) NOT NULL DEFAULT 0 CHECK(observed_charge>=0),
 state text NOT NULL DEFAULT 'prepared' CHECK(state IN ('prepared','ready','dispatched','outcome_unknown','settled','released','not_accepted','conflict')),
 bound_violation boolean NOT NULL DEFAULT false,
 prepared_until timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 dispatched_at timestamptz,
 closed_at timestamptz,
 CHECK((closed_at IS NOT NULL)=(state IN ('settled','released','not_accepted'))),
 CHECK(state NOT IN ('dispatched','outcome_unknown','settled','not_accepted','conflict') OR dispatched_at IS NOT NULL),
 CHECK((run_id IS NULL)=(step_id IS NULL))
);
CREATE INDEX execution_debits_open ON olp.execution_debits(created_at,id) WHERE closed_at IS NULL;
CREATE INDEX execution_debits_owner ON olp.execution_debits(api_key_id,created_at,id);
CREATE INDEX execution_debits_prepared ON olp.execution_debits(prepared_until,id) WHERE state IN ('prepared','ready');

CREATE TABLE olp.execution_debit_owners (
 dispatch_id uuid NOT NULL REFERENCES olp.execution_debits,
 owner_id uuid NOT NULL,
 owner_kind text NOT NULL CHECK(owner_kind IN ('key','group','aggregate','end_user','supply')),
 basis text NOT NULL CHECK(basis IN ('cost','charge')),
 bound numeric(28,12) NOT NULL CHECK(bound>=0),
 daily_window_id bigint NOT NULL CHECK(daily_window_id>=0),
 weekly_window_id bigint NOT NULL CHECK(weekly_window_id>=0),
 monthly_window_id bigint NOT NULL CHECK(monthly_window_id>=0),
 PRIMARY KEY(dispatch_id,owner_id,basis)
);
CREATE INDEX execution_debit_owners_lookup ON olp.execution_debit_owners(owner_id,dispatch_id);

-- A version belongs to the accounting owner, not a worker or a lease. Updating
-- it is serialized with projection so an old snapshot cannot free newer work.
CREATE TABLE olp.execution_debit_versions (
 owner_id uuid PRIMARY KEY,
 version bigint NOT NULL DEFAULT 0 CHECK(version BETWEEN 0 AND 9007199254740991)
);
CREATE TABLE olp.execution_debit_results (
 dispatch_id uuid NOT NULL REFERENCES olp.execution_debits,
 result_digest bytea NOT NULL CHECK(octet_length(result_digest)=32),
 evidence_digest bytea NOT NULL CHECK(octet_length(evidence_digest)=32),
 resolution_digest bytea NOT NULL CHECK(octet_length(resolution_digest)=32),
 event_id uuid NOT NULL UNIQUE,
 cost numeric(28,12) NOT NULL CHECK(cost>=0),
 charge numeric(28,12) NOT NULL CHECK(charge>=0),
 final boolean NOT NULL,
 complete boolean NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(dispatch_id,result_digest)
);
CREATE TABLE olp.execution_debit_result_resolutions (
 dispatch_id uuid NOT NULL,
 resolved_digest bytea NOT NULL,
 evidence_result_digest bytea NOT NULL,
 PRIMARY KEY(dispatch_id,resolved_digest),
 FOREIGN KEY(dispatch_id,resolved_digest) REFERENCES olp.execution_debit_results(dispatch_id,result_digest),
 FOREIGN KEY(dispatch_id,evidence_result_digest) REFERENCES olp.execution_debit_results(dispatch_id,result_digest),
 CHECK(resolved_digest<>evidence_result_digest)
);

CREATE FUNCTION olp.guard_execution_debit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF (NEW.id,NEW.api_key_id,NEW.project_id,NEW.run_id,NEW.step_id,NEW.kind,NEW.affinity,NEW.intent_digest,NEW.claim_id,NEW.currency,NEW.cost_bound,NEW.charge_bound,NEW.prepared_until,NEW.created_at)
 IS DISTINCT FROM
 (OLD.id,OLD.api_key_id,OLD.project_id,OLD.run_id,OLD.step_id,OLD.kind,OLD.affinity,OLD.intent_digest,OLD.claim_id,OLD.currency,OLD.cost_bound,OLD.charge_bound,OLD.prepared_until,OLD.created_at)
 THEN RAISE EXCEPTION 'execution debit affinity is immutable'; END IF;
 IF OLD.closed_at IS NOT NULL AND NEW IS DISTINCT FROM OLD THEN RAISE EXCEPTION 'closed execution debit is immutable'; END IF;
 IF OLD.dispatched_at IS NOT NULL AND (NEW.dispatched_at IS DISTINCT FROM OLD.dispatched_at OR NEW.state IN ('prepared','ready','released'))
 THEN RAISE EXCEPTION 'dispatched execution cannot be refunded or replayed'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER execution_debits_immutable BEFORE UPDATE ON olp.execution_debits FOR EACH ROW EXECUTE FUNCTION olp.guard_execution_debit();
CREATE FUNCTION olp.reject_execution_evidence_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'execution evidence is immutable'; END $$;
CREATE TRIGGER execution_debit_owners_immutable BEFORE UPDATE OR DELETE ON olp.execution_debit_owners FOR EACH ROW EXECUTE FUNCTION olp.reject_execution_evidence_mutation();
CREATE TRIGGER execution_debit_results_immutable BEFORE UPDATE OR DELETE ON olp.execution_debit_results FOR EACH ROW EXECUTE FUNCTION olp.reject_execution_evidence_mutation();
CREATE TRIGGER execution_debit_result_resolutions_immutable BEFORE UPDATE OR DELETE ON olp.execution_debit_result_resolutions FOR EACH ROW EXECUTE FUNCTION olp.reject_execution_evidence_mutation();

-- One indexed relation exposes exact current-window spend for every existing
-- kind of monetary owner. It is never used to turn bounds into usage facts.
CREATE VIEW olp.execution_owner_spend AS
 SELECT api_key_id AS owner_id,window_kind,window_id,accrued,charged,unpriced_attempts,uncharged_attempts,weekly_complete FROM olp.api_key_cost_windows
 UNION ALL SELECT budget_group_id,window_kind,window_id,accrued,charged,unpriced_attempts,uncharged_attempts,weekly_complete FROM olp.budget_group_cost_windows
 UNION ALL SELECT account_id,window_kind,window_id,accrued,charged,unpriced_attempts,uncharged_attempts,weekly_complete FROM olp.aggregate_cost_windows
 UNION ALL SELECT account_id,window_kind,window_id,accrued,charged,unpriced_attempts,uncharged_attempts,weekly_complete FROM olp.end_user_cost_windows
 UNION ALL SELECT owner_id,window_kind,window_id,accrued,accrued,unpriced_attempts,unpriced_attempts,weekly_complete FROM olp.supply_cost_windows;

-- Audited proof of non-acceptance may close unknown exposure; timeout alone
-- cannot. This evidence is immutable and does not manufacture a usage charge.
CREATE TABLE olp.execution_debit_resolutions (
 dispatch_id uuid PRIMARY KEY REFERENCES olp.execution_debits,
 actor_id uuid NOT NULL REFERENCES olp.users,
 evidence_digest bytea NOT NULL CHECK(octet_length(evidence_digest)=32),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TRIGGER execution_debit_resolution_immutable BEFORE UPDATE OR DELETE ON olp.execution_debit_resolutions FOR EACH ROW EXECUTE FUNCTION olp.reject_execution_evidence_mutation();
