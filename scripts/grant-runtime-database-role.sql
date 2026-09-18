\set ON_ERROR_STOP on
BEGIN;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
REVOKE ALL ON SCHEMA olp_go FROM PUBLIC;
GRANT USAGE ON SCHEMA olp_go TO :"runtime_role";
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA olp_go TO :"runtime_role";
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA olp_go TO :"runtime_role";
REVOKE ALL ON olp_go.migrations FROM :"runtime_role";
GRANT SELECT ON olp_go.migrations TO :"runtime_role";
COMMIT;
