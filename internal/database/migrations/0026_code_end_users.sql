ALTER TABLE olp.code_attempts
    ADD COLUMN end_user_digest text NOT NULL DEFAULT ''
    CHECK (end_user_digest = '' OR end_user_digest ~ '^[0-9a-f]{64}$');
ALTER TABLE olp.code_refusals
    ADD COLUMN end_user_digest text NOT NULL DEFAULT ''
    CHECK (end_user_digest = '' OR end_user_digest ~ '^[0-9a-f]{64}$');

CREATE INDEX code_attempts_end_user ON olp.code_attempts(project_id, end_user_digest, id DESC)
    WHERE end_user_digest <> '';
CREATE INDEX code_refusals_end_user ON olp.code_refusals(project_id, end_user_digest, id DESC)
    WHERE end_user_digest <> '';
