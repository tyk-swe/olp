CREATE TABLE olp.capture_configuration (
 singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
 enabled boolean NOT NULL DEFAULT false,
 etag uuid NOT NULL DEFAULT uuidv7(),
 updated_by uuid REFERENCES olp.users,
 updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO olp.capture_configuration(singleton) VALUES(true);

CREATE TABLE olp.capture_policies (
 id uuid PRIMARY KEY,
 project_id uuid REFERENCES olp.projects,
 route_slug text REFERENCES olp.routes(slug),
 sink_id uuid NOT NULL REFERENCES olp.export_sinks,
 sample_ratio text NOT NULL CHECK(sample_ratio ~ '^(0(\.[0-9]{1,9})?|1(\.0{1,9})?)$'),
 include text[] NOT NULL CHECK(cardinality(include) BETWEEN 1 AND 3 AND include <@ ARRAY['input','output','tool_calls']::text[]),
 redact text[] NOT NULL DEFAULT '{}' CHECK(cardinality(redact)=0),
 key_ids uuid[] NOT NULL DEFAULT '{}' CHECK(cardinality(key_ids)<=256),
 end_user_digests text[] NOT NULL DEFAULT '{}' CHECK(cardinality(end_user_digests)<=256),
 max_bytes integer NOT NULL CHECK(max_bytes BETWEEN 1024 AND 1048576),
 enabled boolean NOT NULL DEFAULT true,
 etag uuid NOT NULL,
 created_by uuid NOT NULL REFERENCES olp.users,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 CHECK(project_id IS NOT NULL OR route_slug IS NOT NULL),
 UNIQUE NULLS NOT DISTINCT(project_id,route_slug)
);
ALTER TABLE olp.requests ADD COLUMN payload_captured boolean NOT NULL DEFAULT false;
