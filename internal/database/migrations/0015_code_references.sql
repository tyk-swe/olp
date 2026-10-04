CREATE TABLE olp.code_references (
    route_id uuid NOT NULL REFERENCES olp.code_routes,
    api_key_id uuid NOT NULL REFERENCES olp.api_keys,
    external_id text NOT NULL CHECK (octet_length(external_id) BETWEEN 1 AND 256),
    binding_id uuid NOT NULL REFERENCES olp.code_bindings,
    ambiguous boolean NOT NULL DEFAULT false,
    PRIMARY KEY(route_id,api_key_id,external_id)
);
