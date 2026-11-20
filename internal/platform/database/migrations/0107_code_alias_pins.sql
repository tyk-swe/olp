ALTER TABLE olp.code_pins ADD COLUMN target_model text CHECK (octet_length(target_model) BETWEEN 1 AND 200);
