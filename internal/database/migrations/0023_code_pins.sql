-- A code-mode route's pool may mix subscription families, so a conversation
-- tree keeps one account per model rather than one account: the first
-- admission of each model pins the account that serves it for the tree's
-- lifetime, and a tree holds at most one account of each family. A tree's
-- binding names its first account, which later models prefer when it serves
-- them.
CREATE TABLE olp.code_pins (
    root_id uuid NOT NULL REFERENCES olp.code_bindings,
    model text NOT NULL CHECK (octet_length(model) BETWEEN 1 AND 200),
    account_id uuid NOT NULL REFERENCES olp.code_accounts,
    principal text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(root_id,model)
);
CREATE FUNCTION olp.preserve_code_pin() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    RAISE EXCEPTION 'code pins are durable' USING ERRCODE = '23514';
END $$;
CREATE TRIGGER preserve_code_pin BEFORE UPDATE OR DELETE ON olp.code_pins
    FOR EACH ROW EXECUTE FUNCTION olp.preserve_code_pin();
