package release

func PluginColumn(alias string) string {
	return "(SELECT jsonb_build_object('manifest', pl.manifest, 'unconfined', pl.executable IS NOT NULL) FROM olp.plugins pl WHERE " + alias + ".configuration->>'kind'='plugin' AND pl.digest=" + alias + ".configuration->>'profile_revision')"
}
