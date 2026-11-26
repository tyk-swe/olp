package identity

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/access/httpkit"
)

const authenticationCapabilitiesSQL = `SELECT COALESCE((SELECT value='true' FROM olp.settings WHERE key='auth.local_login_enabled'),true),
 COALESCE((SELECT (document->>'enabled')::boolean FROM olp.oidc_configuration WHERE singleton),false),
 COALESCE((SELECT (document->>'enabled')::boolean FROM olp.saml_configuration WHERE singleton),false),
 COALESCE((SELECT enabled FROM olp.capture_configuration WHERE singleton),false)`

type authCapabilitiesResponse struct {
	LocalLoginEnabled            bool `json:"local_login_enabled"`
	OIDCLoginEnabled             bool `json:"oidc_login_enabled"`
	SAMLLoginEnabled             bool `json:"saml_login_enabled"`
	GatewayAvailable             bool `json:"gateway_available"`
	LimitsEnforced               bool `json:"limits_enforced"`
	ManagementNetworkRestricted  bool `json:"management_network_restricted"`
	RetentionEnforced            bool `json:"retention_enforced"`
	NotificationsActive          bool `json:"notifications_active"`
	PayloadCaptureActive         bool `json:"payload_capture_active"`
	ResponseCacheConfigured      bool `json:"response_cache_configured"`
	SemanticCacheVectorsUnsealed bool `json:"semantic_cache_vectors_unsealed"`
}

func (s *accountService) capabilities(r *http.Request) (httpkit.Reply, error) {
	var local, oidc, samlEnabled, captureEnabled bool
	err := s.access.Pool.QueryRow(r.Context(), authenticationCapabilitiesSQL).Scan(&local, &oidc, &samlEnabled, &captureEnabled)
	return httpkit.OK(authCapabilitiesResponse{
		LocalLoginEnabled:            local && !s.access.LocalLoginDisabled(),
		OIDCLoginEnabled:             oidc,
		SAMLLoginEnabled:             samlEnabled,
		GatewayAvailable:             true,
		LimitsEnforced:               s.access.LimitsEnforced(),
		ManagementNetworkRestricted:  s.managementNetworkRestricted,
		RetentionEnforced:            s.access.LimitsEnforced(),
		NotificationsActive:          true,
		PayloadCaptureActive:         captureEnabled,
		ResponseCacheConfigured:      s.responseCacheConfigured,
		SemanticCacheVectorsUnsealed: true,
	}), err
}
