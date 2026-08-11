-- Give linked identities stable management identifiers without exposing an
-- issuer/subject pair in URLs. PostgreSQL 18's uuidv7() keeps the clean-break
-- UUIDv7 contract for both existing fixtures and all future links.
ALTER TABLE oidc_identities
    ADD COLUMN id uuid NOT NULL DEFAULT uuidv7(),
    ADD CONSTRAINT oidc_identities_id_unique UNIQUE (id);

CREATE INDEX oidc_identities_user_created_idx
    ON oidc_identities (user_id, created_at, id);
