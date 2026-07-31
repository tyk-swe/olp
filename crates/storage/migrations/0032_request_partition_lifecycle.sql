-- Keep request metadata writes out of the default partition without rewriting
-- legacy rows whose attempts still reference the partitioned parent.
CREATE TABLE request_partition_state (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    managed_from timestamptz NOT NULL
);

INSERT INTO request_partition_state (singleton, managed_from)
VALUES (
    true,
    (
        date_trunc('month', CURRENT_TIMESTAMP AT TIME ZONE 'UTC')
        + interval '1 month'
    ) AT TIME ZONE 'UTC'
);

CREATE TABLE request_partitions (
    partition_start timestamptz PRIMARY KEY,
    partition_end timestamptz NOT NULL,
    partition_name text NOT NULL UNIQUE,
    CHECK (
        partition_end = (
            partition_start AT TIME ZONE 'UTC' + interval '1 month'
        ) AT TIME ZONE 'UTC'
    ),
    CHECK (partition_name ~ '^requests_y[0-9]{4}m[0-9]{2}$')
);

-- Promote the existing per-default indexes into partitioned indexes. New
-- partitions then receive matching indexes automatically.
CREATE INDEX requests_started_at_idx ON ONLY requests(started_at DESC);
ALTER INDEX requests_started_at_idx
    ATTACH PARTITION requests_default_started_at_idx;
CREATE INDEX requests_route_started_at_idx ON ONLY requests(route_slug, started_at DESC);
ALTER INDEX requests_route_started_at_idx
    ATTACH PARTITION requests_default_route_idx;

CREATE FUNCTION olp_maintain_request_partitions(
    reference_time timestamptz,
    retention_cutoff timestamptz
)
RETURNS TABLE (
    created_count bigint,
    dropped_count bigint
)
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $$
DECLARE
    month_start timestamptz;
    month_end timestamptz;
    month_limit timestamptz;
    managed_from timestamptz;
    child_name text;
    child record;
    child_has_attempts boolean;
BEGIN
    created_count := 0;
    dropped_count := 0;

    SELECT state.managed_from
      INTO managed_from
      FROM public.request_partition_state state
     WHERE state.singleton;

    -- Keep the next three UTC months ready. Also recover a missed current
    -- month when it is still empty; existing default rows are never rewritten.
    month_start := GREATEST(
        managed_from,
        date_trunc('month', reference_time AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'
    );
    month_limit := (
        date_trunc('month', reference_time AT TIME ZONE 'UTC')
        + interval '3 months'
    ) AT TIME ZONE 'UTC';
    WHILE month_start <= month_limit LOOP
        month_end := (
            month_start AT TIME ZONE 'UTC' + interval '1 month'
        ) AT TIME ZONE 'UTC';
        child_name := format('requests_y%sm%s',
            to_char(month_start AT TIME ZONE 'UTC', 'YYYY'),
            to_char(month_start AT TIME ZONE 'UTC', 'MM'));

        IF NOT EXISTS (
            SELECT 1
              FROM public.request_partitions registry
             WHERE registry.partition_start = month_start
        ) THEN
            -- Avoid taking an ACCESS EXCLUSIVE lock every minute for a month
            -- that has already spilled. Recheck under the lock before attach.
            IF NOT EXISTS (
                SELECT 1
                  FROM public.requests_default spill
                 WHERE spill.started_at >= month_start
                   AND spill.started_at < month_end
            ) THEN
                SET LOCAL lock_timeout = '2s';
                LOCK TABLE public.requests IN ACCESS EXCLUSIVE MODE;
                LOCK TABLE public.requests_default IN ACCESS EXCLUSIVE MODE;
                IF NOT EXISTS (
                    SELECT 1
                      FROM public.requests_default spill
                     WHERE spill.started_at >= month_start
                       AND spill.started_at < month_end
                ) THEN
                    IF to_regclass(format('public.%I', child_name)) IS NOT NULL THEN
                        RAISE EXCEPTION 'request partition name % already exists', child_name;
                    END IF;

                    EXECUTE format(
                        'CREATE TABLE public.%I PARTITION OF public.requests '
                        'FOR VALUES FROM (%L) TO (%L)',
                        child_name,
                        month_start,
                        month_end
                    );
                    INSERT INTO public.request_partitions
                        (partition_start, partition_end, partition_name)
                    VALUES (month_start, month_end, child_name);
                    created_count := created_count + 1;
                END IF;
            END IF;
        END IF;
        month_start := month_end;
    END LOOP;

    FOR child IN
        SELECT registry.partition_start,
               registry.partition_end,
               registry.partition_name
         FROM public.request_partitions registry
         WHERE registry.partition_end <= retention_cutoff
         ORDER BY registry.partition_start
         LIMIT 3
    LOOP
        SET LOCAL lock_timeout = '2s';
        LOCK TABLE public.requests IN ACCESS EXCLUSIVE MODE;
        EXECUTE format(
            'LOCK TABLE public.%I IN ACCESS EXCLUSIVE MODE',
            child.partition_name
        );
        -- The parent FK prevents detach while attempts still reference this
        -- child. Drain them in bounded batches and leave the partition
        -- attached until a later maintenance tick removes the final batch.
        EXECUTE format(
            'DELETE FROM public.attempts attempt WHERE attempt.ctid IN ('
            'SELECT candidate.ctid FROM public.attempts candidate '
            'JOIN public.%I request '
            'ON request.id = candidate.request_id '
            'AND request.started_at = candidate.request_started_at '
            'LIMIT 10000 FOR UPDATE OF candidate SKIP LOCKED)',
            child.partition_name
        );
        EXECUTE format(
            'SELECT EXISTS ('
            'SELECT 1 FROM public.attempts attempt '
            'JOIN public.%I request '
            'ON request.id = attempt.request_id '
            'AND request.started_at = attempt.request_started_at)',
            child.partition_name
        ) INTO child_has_attempts;
        IF child_has_attempts THEN
            CONTINUE;
        END IF;
        EXECUTE format(
            'ALTER TABLE public.requests DETACH PARTITION public.%I',
            child.partition_name
        );
        EXECUTE format('DROP TABLE public.%I', child.partition_name);
        DELETE FROM public.request_partitions registry
         WHERE registry.partition_start = child.partition_start;
        dropped_count := dropped_count + 1;
    END LOOP;

    RETURN NEXT;
END;
$$;

REVOKE ALL ON FUNCTION olp_maintain_request_partitions(timestamptz, timestamptz)
    FROM PUBLIC;

-- Seed partitions ahead of the first maintenance tick.
SELECT * FROM olp_maintain_request_partitions(
    CURRENT_TIMESTAMP,
    '-infinity'::timestamptz
);
