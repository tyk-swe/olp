-- Replay-safe management mutations keep only a request fingerprint in the
-- clear. The original HTTP response (including one-time bearer material) is
-- stored as an AES-256-GCM envelope and can be opened only with the mounted
-- master-key keyring.
ALTER TABLE idempotency_records
    ADD COLUMN request_fingerprint bytea,
    ADD COLUMN replay_ciphertext bytea,
    ADD COLUMN replay_nonce bytea,
    ADD COLUMN replay_key_version integer;

ALTER TABLE idempotency_records
    ADD CONSTRAINT idempotency_request_fingerprint_size
        CHECK (request_fingerprint IS NULL OR octet_length(request_fingerprint) = 32),
    ADD CONSTRAINT idempotency_replay_envelope_complete
        CHECK (
            (replay_ciphertext IS NULL AND replay_nonce IS NULL AND replay_key_version IS NULL)
            OR
            (replay_ciphertext IS NOT NULL AND octet_length(replay_ciphertext) >= 16
             AND replay_nonce IS NOT NULL
             AND octet_length(replay_nonce) = 12 AND replay_key_version > 0)
        ),
    ADD CONSTRAINT idempotency_in_progress_has_no_replay
        CHECK (
            state <> 'in_progress'
            OR (replay_ciphertext IS NULL AND replay_nonce IS NULL AND replay_key_version IS NULL)
        ),
    ADD CONSTRAINT idempotency_replay_resource_is_encrypted
        CHECK (
            request_fingerprint IS NULL OR resource_id IS NULL
        );
