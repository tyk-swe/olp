-- An event describes exactly one request, and a request has exactly one
-- terminal event. The composite primary key alone permits either identity to
-- be reused concurrently or after raw facts have been rolled up.
CREATE UNIQUE INDEX request_metadata_event_receipts_event_identity_idx
    ON olp.request_metadata_event_receipts (event_id);
CREATE UNIQUE INDEX request_metadata_event_receipts_request_identity_idx
    ON olp.request_metadata_event_receipts (request_id);
