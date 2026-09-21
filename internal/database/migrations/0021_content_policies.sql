ALTER TABLE olp_go.route_drafts ADD COLUMN content_policy jsonb;
ALTER TABLE olp_go.route_revisions ADD COLUMN content_policy jsonb;
ALTER TABLE olp_go.requests ADD COLUMN policy_decisions jsonb NOT NULL DEFAULT '[]'::jsonb
 CHECK(jsonb_typeof(policy_decisions)='array' AND octet_length(policy_decisions::text)<=16384);
