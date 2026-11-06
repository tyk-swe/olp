ALTER TABLE olp.code_attempts
    ADD COLUMN allowance_charge jsonb CHECK (jsonb_typeof(allowance_charge)='object');
