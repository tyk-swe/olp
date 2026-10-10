ALTER TABLE olp.installation
    ADD COLUMN logo text NOT NULL DEFAULT '',
    ADD COLUMN branding_etag uuid NOT NULL DEFAULT gen_random_uuid();
