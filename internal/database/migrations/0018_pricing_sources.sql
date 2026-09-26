CREATE TABLE olp.pricing_sources (
    id uuid PRIMARY KEY,
    name text NOT NULL UNIQUE,
    url text NOT NULL,
    enabled boolean NOT NULL DEFAULT true,
    etag uuid NOT NULL,
    created_by uuid NOT NULL REFERENCES olp.users,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE olp.pricing_source_snapshots (
    id uuid PRIMARY KEY,
    source_id uuid NOT NULL REFERENCES olp.pricing_sources,
    sha256 text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    document jsonb NOT NULL CHECK (octet_length(document::text) <= 4194304),
    fetched_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (source_id, sha256)
);
ALTER TABLE olp.pricing_revisions
    ADD COLUMN source_snapshot_id uuid REFERENCES olp.pricing_source_snapshots;
