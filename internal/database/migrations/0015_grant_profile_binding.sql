-- Enrollment must bind a grant to both its plugin build and its profile.
-- Older versions did not record the enrolling profile; do not guess it from
-- mutable drafts. They require re-enrollment before new validation/activation.
ALTER TABLE olp.provider_credentials ADD COLUMN profile_id text;
