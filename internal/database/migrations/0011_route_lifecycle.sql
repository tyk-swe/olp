ALTER TABLE olp.routes
    ADD COLUMN state text NOT NULL DEFAULT 'active' CHECK (state IN ('active','retired')),
    ADD COLUMN etag uuid,
    ADD COLUMN retired_at timestamptz,
    ADD COLUMN retired_by uuid REFERENCES olp.users;
UPDATE olp.routes SET etag=id WHERE etag IS NULL;
ALTER TABLE olp.routes ALTER COLUMN etag SET NOT NULL;
ALTER TABLE olp.routes ADD CONSTRAINT routes_retirement_check CHECK (
    (state='active' AND retired_at IS NULL AND retired_by IS NULL)
 OR (state='retired' AND retired_at IS NOT NULL AND retired_by IS NOT NULL));
