ALTER TABLE olp.code_references ADD COLUMN account_id uuid REFERENCES olp.code_accounts;

WITH owners AS (
    SELECT b.root_id,min(a.account_id::text)::uuid AS account_id
    FROM olp.code_bindings b
    JOIN olp.code_attempts a ON a.binding_id=b.id AND a.state IN ('uncertain','settled','bound_violation')
    GROUP BY b.root_id
    HAVING count(DISTINCT a.account_id)=1
)
UPDATE olp.code_references x SET account_id=o.account_id
FROM olp.code_bindings b JOIN owners o ON o.root_id=b.root_id
WHERE x.binding_id=b.id;

UPDATE olp.code_references SET ambiguous=true WHERE account_id IS NULL;
