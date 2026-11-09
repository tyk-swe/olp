package access

const authenticationCapabilitiesSQL = `SELECT COALESCE((SELECT value='true' FROM olp.settings WHERE key='auth.local_login_enabled'),true),
 COALESCE((SELECT (document->>'enabled')::boolean FROM olp.oidc_configuration WHERE singleton),false),
 COALESCE((SELECT (document->>'enabled')::boolean FROM olp.saml_configuration WHERE singleton),false),
 COALESCE((SELECT enabled FROM olp.capture_configuration WHERE singleton),false)`
