ALTER TABLE olp.mcp_tool_calls
 ADD COLUMN agent_principal_id uuid REFERENCES olp.api_keys(id),
 ADD COLUMN member_principal_id uuid REFERENCES olp.users(id),
 ADD COLUMN delegation_pair_digest text,
 ADD CONSTRAINT mcp_tool_calls_delegation_pair CHECK (
  (agent_principal_id IS NULL AND delegation_pair_digest IS NULL AND member_principal_id IS NULL) OR
  (agent_principal_id IS NOT NULL AND delegation_pair_digest IS NOT NULL AND delegation_pair_digest ~ '^[0-9a-f]{64}$')
 );
