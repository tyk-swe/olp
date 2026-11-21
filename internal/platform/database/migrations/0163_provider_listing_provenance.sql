ALTER TABLE olp.provider_models
 ADD COLUMN discovered_listing_endpoint text,
 ADD COLUMN discovered_listing_api_version text,
 ADD COLUMN discovered_listing_credential_id uuid,
 ADD COLUMN discovered_listing_credential_version integer,
 ADD COLUMN discovered_listing_at timestamptz,
 ADD CONSTRAINT provider_models_listing_provenance_check CHECK (
  num_nonnulls(discovered_listing_endpoint, discovered_listing_api_version, discovered_listing_credential_id, discovered_listing_credential_version, discovered_listing_at) IN (0, 5)
 );
