\set ON_ERROR_STOP on
BEGIN;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
REVOKE ALL ON SCHEMA olp_v3 FROM PUBLIC;
GRANT USAGE ON SCHEMA olp_v3 TO :"runtime_role";
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA olp_v3 TO :"runtime_role";
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA olp_v3 TO :"runtime_role";
REVOKE ALL ON olp_v3._sqlx_migrations, olp_v3.installation_identity FROM :"runtime_role";
GRANT SELECT ON olp_v3.installation_identity TO :"runtime_role";
COMMIT;
