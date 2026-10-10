-- Local coordination tasks need local liveness even though durable accounting
-- and the installation-wide worker summary remain shared in PostgreSQL.
CREATE TABLE olp.regional_worker_task_health (
    LIKE olp.worker_task_health INCLUDING DEFAULTS INCLUDING CONSTRAINTS,
    region text NOT NULL CHECK (length(region) BETWEEN 1 AND 100),
    PRIMARY KEY (region, task)
);
