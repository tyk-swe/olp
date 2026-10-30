ALTER TABLE olp.code_paid_bindings ADD COLUMN subscription_root_id uuid REFERENCES olp.code_bindings;
