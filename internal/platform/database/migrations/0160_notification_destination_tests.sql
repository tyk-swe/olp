CREATE TABLE olp.notification_destination_tests (
 id uuid PRIMARY KEY,
 destination_id uuid NOT NULL REFERENCES olp.notification_destinations ON DELETE CASCADE,
 action_id uuid NOT NULL,
 claimed_at timestamptz NOT NULL,
 expires_at timestamptz NOT NULL,
 finished_at timestamptz,
 status text NOT NULL DEFAULT 'unconfirmed' CHECK(status IN ('delivered','failed','unconfirmed')),
 failure text,
 UNIQUE(destination_id,action_id)
);
CREATE INDEX notification_destination_tests_recent ON olp.notification_destination_tests(destination_id,claimed_at);
