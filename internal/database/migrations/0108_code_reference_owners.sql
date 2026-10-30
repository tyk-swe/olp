ALTER TABLE olp.code_references ADD COLUMN account_id uuid REFERENCES olp.code_accounts;

WITH owners AS (
    SELECT x.route_id,x.api_key_id,x.external_id,min(a.account_id::text)::uuid AS account_id
    FROM olp.code_references x
    JOIN olp.code_bindings owner ON owner.id=x.binding_id
    JOIN olp.code_bindings member ON member.root_id=owner.root_id
    JOIN olp.code_attempts a ON a.binding_id=member.id AND a.state IN ('uncertain','settled','bound_violation')
    GROUP BY x.route_id,x.api_key_id,x.external_id
    HAVING count(DISTINCT a.account_id)=1
)
UPDATE olp.code_references x SET account_id=o.account_id
FROM owners o WHERE x.route_id=o.route_id AND x.api_key_id=o.api_key_id AND x.external_id=o.external_id;

UPDATE olp.code_references SET ambiguous=true WHERE account_id IS NULL;
