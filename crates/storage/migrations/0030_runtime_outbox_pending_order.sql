CREATE INDEX transactional_outbox_pending_order_idx
    ON transactional_outbox(created_at, id)
    WHERE published_at IS NULL;
