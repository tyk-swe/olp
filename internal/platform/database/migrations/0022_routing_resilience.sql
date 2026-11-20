-- Adaptive routing: a route draft and revision carry their fallbacks,
-- selectors, retry policy, session affinity and spend cap as one document,
-- whose absence leaves a route exactly as it was.
ALTER TABLE olp.route_drafts
    ADD COLUMN behavior jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(behavior) = 'object');
ALTER TABLE olp.route_revisions
    ADD COLUMN behavior jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(behavior) = 'object');

-- Supply-side cost caps: what each capped provider connection, credential
-- slot and route may spend. Publication rewrites the rows from the release it
-- installs, so reconciliation keeps windows current for exactly the owners a
-- serving release caps, without reading releases itself.
CREATE TABLE olp.supply_budgets (
    owner_id uuid PRIMARY KEY,
    owner_kind text NOT NULL CHECK (owner_kind IN ('connection','slot','route')),
    daily_cost_limit numeric(24,12) CHECK (daily_cost_limit > 0),
    monthly_cost_limit numeric(24,12) CHECK (monthly_cost_limit > 0),
    CHECK (daily_cost_limit IS NOT NULL OR monthly_cost_limit IS NOT NULL)
);

-- Spend accrued against supply caps, in the same fixed UTC windows and exact
-- decimals as key budgets. An attempt accrues to the owners that capped it when
-- it was admitted, so a cap counts spend from the moment it is declared.
CREATE TABLE olp.supply_cost_windows (
    owner_id uuid NOT NULL,
    window_kind text NOT NULL CHECK (window_kind IN ('day','month')),
    window_id bigint NOT NULL CHECK (window_id >= 0),
    accrued numeric(28,12) NOT NULL CHECK (accrued >= 0),
    unpriced_attempts bigint NOT NULL CHECK (unpriced_attempts >= 0),
    CONSTRAINT supply_cost_windows_unpriced_scope_check
        CHECK (window_kind = 'month' OR unpriced_attempts = 0),
    PRIMARY KEY (owner_id, window_kind, window_id)
);

-- Requests the gateway makes on its own account: shadow mirrors, selector
-- classifier calls and health probes. Shadow and probe requests belong to the
-- installation and carry no API key, so they spend from no key budget; a
-- shadow or classifier request names the caller request it was made for.
ALTER TABLE olp.requests
    ALTER COLUMN api_key_id DROP NOT NULL,
    ADD COLUMN origin text NOT NULL DEFAULT 'caller'
        CHECK (origin IN ('caller','shadow','classifier','probe')),
    ADD COLUMN parent_request_id uuid,
    ADD CONSTRAINT requests_origin_key_check
        CHECK ((api_key_id IS NULL) = (origin IN ('shadow','probe'))),
    ADD CONSTRAINT requests_origin_parent_check
        CHECK ((parent_request_id IS NOT NULL) = (origin IN ('shadow','classifier')));
CREATE INDEX requests_parent_idx ON olp.requests (parent_request_id) WHERE parent_request_id IS NOT NULL;
ALTER TABLE olp.attempt_usage_facts ALTER COLUMN api_key_id DROP NOT NULL;

-- Active health probes checkpoint like every other worker task.
ALTER TABLE olp.worker_task_health DROP CONSTRAINT worker_task_health_task_check;
ALTER TABLE olp.worker_task_health
    ADD CONSTRAINT worker_task_health_task_check CHECK (task IN ('request_metadata_consumer', 'maintenance',
        'cost_reconciliation', 'request_metadata_gateway_epoch_detection', 'media_reconciliation',
        'notification_delivery', 'grant_refresh', 'health_probes'));

-- A route template turns certified models into ordinary routes. Generated
-- routes keep no tie to the template beyond the record of what it created,
-- so editing or deleting a template never changes a published route.
CREATE TABLE olp.route_templates (
    id uuid PRIMARY KEY,
    name text NOT NULL UNIQUE CHECK (name ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    provider_selector text NOT NULL,
    model_filter text NOT NULL,
    slug_pattern text NOT NULL,
    overall_timeout_ms integer NOT NULL,
    max_attempts integer NOT NULL,
    fidelity jsonb NOT NULL CHECK (
        fidelity IN ('{"mode": "strict"}'::jsonb, '{"mode": "transformed"}'::jsonb)
    ),
    routing_policy jsonb,
    auto_publish boolean NOT NULL DEFAULT false,
    project_id uuid REFERENCES olp.projects,
    etag uuid NOT NULL,
    created_by uuid NOT NULL REFERENCES olp.users,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Each certified model a template has placed on a route, so a later
-- activation neither repeats it nor recreates a route an operator deleted.
CREATE TABLE olp.route_template_routes (
    template_id uuid NOT NULL REFERENCES olp.route_templates ON DELETE CASCADE,
    provider_model_id uuid NOT NULL,
    provider_id uuid NOT NULL,
    upstream_model text NOT NULL,
    route_slug text NOT NULL,
    draft_id uuid REFERENCES olp.route_drafts ON DELETE SET NULL,
    outcome text NOT NULL CHECK (outcome IN ('draft','published')),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (template_id, provider_model_id)
);

CREATE INDEX route_template_routes_slug_idx ON olp.route_template_routes (template_id, route_slug);

-- A selector's savings compare each attempt's cost with what its usage would
-- have cost on the most expensive target the selector avoided, priced when the
-- attempt was accounted. Attempts without a comparable price carry no
-- baseline, so the report never invents usage.
ALTER TABLE olp.attempt_usage_facts
    ADD COLUMN selector text CHECK (selector ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    ADD COLUMN baseline_cost numeric(28, 12) CHECK (baseline_cost >= 0),
    ADD CONSTRAINT attempt_usage_facts_baseline_check CHECK (baseline_cost IS NULL OR selector IS NOT NULL);
CREATE INDEX attempt_usage_facts_selector_idx ON olp.attempt_usage_facts (route_slug, selector, observed_at)
    WHERE selector IS NOT NULL;
