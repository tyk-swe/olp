-- A promoted draft carries an explicit disclosure choice into activation.
-- Ordinary drafts inherit the published route's independent catalog setting.
ALTER TABLE olp.route_drafts ADD COLUMN catalog_expose_upstream_models boolean;

CREATE TABLE olp.model_catalog_project_settings (
    project_id uuid PRIMARY KEY REFERENCES olp.projects ON DELETE CASCADE,
    enabled boolean NOT NULL DEFAULT false,
    prices_public boolean NOT NULL DEFAULT false,
    etag uuid NOT NULL DEFAULT gen_random_uuid()
);

CREATE TABLE olp.model_catalog_route_settings (
    route_id uuid PRIMARY KEY REFERENCES olp.routes ON DELETE CASCADE,
    expose_upstream_models boolean NOT NULL DEFAULT false,
    etag uuid NOT NULL DEFAULT gen_random_uuid()
);
