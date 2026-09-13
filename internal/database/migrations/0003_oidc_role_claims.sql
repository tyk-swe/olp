-- Verified authorization inputs let configuration changes protect OIDC-only
-- owners using the same role mapping as sign-in. Existing identities acquire
-- these private facts on their next successful OIDC authentication.
ALTER TABLE olp_go.oidc_identities ADD COLUMN role_claims jsonb;
