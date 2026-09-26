ALTER TABLE olp.route_drafts ADD COLUMN content_policy jsonb;
ALTER TABLE olp.route_revisions ADD COLUMN content_policy jsonb;
ALTER TABLE olp.requests ADD COLUMN policy_decisions jsonb NOT NULL DEFAULT '[]'::jsonb
 CHECK(jsonb_typeof(policy_decisions)='array' AND octet_length(policy_decisions::text)<=16384);
