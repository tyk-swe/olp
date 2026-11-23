CREATE TABLE olp.runtime_shared_quota_retirements (
    sequence bigint NOT NULL REFERENCES olp.runtime_releases(sequence) ON DELETE CASCADE,
    project_id text NOT NULL,
    group_name text NOT NULL,
    PRIMARY KEY (sequence, project_id, group_name)
);

CREATE FUNCTION olp.snapshot_shared_quota_groups(snapshot json)
RETURNS TABLE (project_id text, group_name text)
LANGUAGE sql IMMUTABLE STRICT AS $$
    SELECT DISTINCT provider.value->>'project_id', quota.value->>'group'
    FROM json_each(CASE WHEN json_typeof(snapshot->'providers') = 'object'
                        THEN snapshot->'providers' ELSE '{}'::json END) provider
    CROSS JOIN LATERAL json_array_elements(
        CASE WHEN json_typeof(provider.value->'shared_token_quotas') = 'array'
             THEN provider.value->'shared_token_quotas' ELSE '[]'::json END
    ) quota
    WHERE provider.value->>'project_id' IS NOT NULL AND quota.value->>'group' IS NOT NULL
$$;

CREATE FUNCTION olp.record_shared_quota_retirements() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO olp.runtime_shared_quota_retirements (sequence, project_id, group_name)
    SELECT NEW.sequence, retired.project_id, retired.group_name
    FROM (
        SELECT * FROM olp.snapshot_shared_quota_groups((
            SELECT snapshot FROM olp.runtime_releases
            WHERE sequence < NEW.sequence ORDER BY sequence DESC LIMIT 1
        ))
        EXCEPT
        SELECT * FROM olp.snapshot_shared_quota_groups(NEW.snapshot)
    ) retired;
    RETURN NEW;
END
$$;

CREATE TRIGGER record_shared_quota_retirements AFTER INSERT ON olp.runtime_releases
FOR EACH ROW EXECUTE FUNCTION olp.record_shared_quota_retirements();

WITH releases AS (
    SELECT snapshot, sequence, lead(sequence) OVER (ORDER BY sequence) AS next_sequence
    FROM olp.runtime_releases
), groups AS MATERIALIZED (
    SELECT r.sequence, r.next_sequence, g.project_id, g.group_name
    FROM releases r CROSS JOIN LATERAL olp.snapshot_shared_quota_groups(r.snapshot) g
)
INSERT INTO olp.runtime_shared_quota_retirements (sequence, project_id, group_name)
SELECT previous.next_sequence, previous.project_id, previous.group_name
FROM groups previous
WHERE previous.next_sequence IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM groups next
      WHERE next.sequence = previous.next_sequence
        AND next.project_id = previous.project_id AND next.group_name = previous.group_name
  );
