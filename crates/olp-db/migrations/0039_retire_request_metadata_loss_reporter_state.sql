-- The loss reporter's crash-safe checkpoint is `request_metadata_gateway_epochs`.
-- Each checkpoint locks the epoch row, reads its `dropped`/`abandoned`
-- high-water marks, and commits the derived gap together with the new marks in
-- one transaction, so a restarted reporter cannot report the same loss twice.
--
-- `request_metadata_loss_reporter_state` held the same two counters keyed by
-- gateway instance alone. Nothing ever read it, and its per-instance key
-- carries counters across process epochs, which a delta over a fresh process
-- must not do. Retire it.
DROP TABLE IF EXISTS request_metadata_loss_reporter_state;
