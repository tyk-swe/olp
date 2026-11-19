ALTER TABLE olp.installation ADD COLUMN budget_policy jsonb,
 ADD COLUMN budget_etag uuid NOT NULL DEFAULT uuidv7(),
 ADD CONSTRAINT installation_budget_policy_object CHECK (budget_policy IS NULL OR jsonb_typeof(budget_policy)='object');
ALTER TABLE olp.projects ADD COLUMN budget_policy jsonb,
 ADD CONSTRAINT project_budget_policy_object CHECK (budget_policy IS NULL OR jsonb_typeof(budget_policy)='object');

-- Owners persist across disabling/re-enabling a policy. No spend reset is implied.
CREATE TABLE olp.aggregate_budget_accounts (
 id uuid PRIMARY KEY,
 installation_id uuid UNIQUE REFERENCES olp.installation(id) ON DELETE CASCADE,
 project_id uuid UNIQUE REFERENCES olp.projects(id) ON DELETE CASCADE,
 initialized boolean NOT NULL DEFAULT false,
 CHECK (num_nonnulls(installation_id,project_id)=1)
);
CREATE TABLE olp.aggregate_cost_windows (
 account_id uuid NOT NULL REFERENCES olp.aggregate_budget_accounts(id) ON DELETE CASCADE,
 window_kind text NOT NULL CHECK (window_kind IN ('day','month')),
 window_id bigint NOT NULL CHECK (window_id>=0),
 accrued numeric(28,12) NOT NULL DEFAULT 0 CHECK (accrued>=0),
 unpriced_attempts bigint NOT NULL DEFAULT 0 CHECK (unpriced_attempts>=0),
 CHECK (window_kind='month' OR unpriced_attempts=0),
 PRIMARY KEY (account_id,window_kind,window_id)
);
