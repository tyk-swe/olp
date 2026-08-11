CREATE TABLE installation_identity (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    id uuid NOT NULL UNIQUE DEFAULT gen_random_uuid(),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

INSERT INTO installation_identity (singleton) VALUES (true);
