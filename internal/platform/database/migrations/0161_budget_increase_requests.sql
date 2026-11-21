CREATE TABLE olp.budget_increase_requests (
 id uuid PRIMARY KEY,
 project_id uuid NOT NULL REFERENCES olp.projects(id) ON DELETE CASCADE,
 owner_id uuid NOT NULL,
 window_kind text NOT NULL CHECK(window_kind IN ('day','week','month')),
 window_id bigint NOT NULL CHECK(window_id>=0),
 window_ends_at timestamptz NOT NULL,
 base_cap numeric(24,12) NOT NULL CHECK(base_cap>0),
 amount numeric(24,12) NOT NULL CHECK(amount>0),
 reason text NOT NULL CHECK(length(reason) BETWEEN 1 AND 256),
 status text NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','approved','declined')),
 requested_by uuid NOT NULL REFERENCES olp.users(id),
 decided_by uuid REFERENCES olp.users(id),
 decided_at timestamptz,
 increase_id uuid REFERENCES olp.budget_increases(id),
 etag uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 CHECK((status='pending')=(decided_at IS NULL)),
 CHECK((status='pending')=(decided_by IS NULL)),
 CHECK((status='approved')=(increase_id IS NOT NULL)),
 CHECK(status<>'approved' OR decided_by<>requested_by)
);
CREATE UNIQUE INDEX budget_increase_requests_increase ON olp.budget_increase_requests(increase_id) WHERE increase_id IS NOT NULL;
CREATE INDEX budget_increase_requests_project ON olp.budget_increase_requests(project_id,id DESC);
CREATE INDEX budget_increase_requests_pending ON olp.budget_increase_requests(owner_id,window_kind,window_id) WHERE status='pending';
