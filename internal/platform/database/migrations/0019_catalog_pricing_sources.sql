-- A pricing source may be a signed reference catalog: the catalog this release
-- ships when the source names no URL, or a catalog fetched beside its detached
-- signature. Refresh verifies the signature against the keys the binary
-- trusts before any price is read, and records the newest catalog a source
-- took so an older signed catalog can never roll its prices back. Snapshots
-- keep which catalog, published when and signed by which key, they came from.
ALTER TABLE olp.pricing_sources
    ADD COLUMN format text NOT NULL DEFAULT 'prices' CHECK (format IN ('prices', 'catalog')),
    ALTER COLUMN url DROP NOT NULL,
    ADD COLUMN catalog_published_at timestamptz,
    ADD CONSTRAINT pricing_sources_url_format CHECK (url IS NOT NULL OR format = 'catalog');

ALTER TABLE olp.pricing_source_snapshots
    ADD COLUMN catalog_sha256 text CHECK (catalog_sha256 ~ '^[0-9a-f]{64}$'),
    ADD COLUMN catalog_published_at timestamptz,
    ADD COLUMN catalog_key_id text,
    ADD CONSTRAINT pricing_source_snapshots_catalog
        CHECK (num_nulls(catalog_sha256, catalog_published_at, catalog_key_id) IN (0, 3));
