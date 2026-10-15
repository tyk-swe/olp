package runtime

const PluginColumn = "(SELECT jsonb_build_object('manifest', pl.manifest, 'unconfined', pl.executable IS NOT NULL) FROM olp.plugins pl WHERE r.configuration->>'kind'='plugin' AND pl.digest=r.configuration->>'profile_revision')"
