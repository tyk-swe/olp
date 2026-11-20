ALTER TABLE olp.code_accounts
    ADD COLUMN allowance_read_requested_at timestamptz,
    ADD COLUMN allowance_read_at timestamptz;

ALTER TABLE olp.worker_task_health DROP CONSTRAINT worker_task_health_task_check,
    ADD CONSTRAINT worker_task_health_task_check CHECK (task IN ('request_metadata_consumer','maintenance','cost_reconciliation','request_metadata_gateway_epoch_detection','media_reconciliation','notification_delivery','grant_refresh','health_probes','export_delivery','managed_export_delivery','focus_export','billing_import','batch_settlement','execution_debits','retained_settlement','code_allowance_read'));
