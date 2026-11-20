ALTER TABLE olp.code_accounts
    ADD COLUMN draining boolean NOT NULL DEFAULT false;

CREATE INDEX code_pins_account ON olp.code_pins (account_id);
CREATE INDEX code_decisions_account_activity ON olp.code_decisions (chosen_account_id, created_at DESC);
