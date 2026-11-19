package main

import (
	"net/http"
	"net/netip"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/identity"

	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/consumerauth"
	"github.com/tyk-swe/olp/internal/domain/surface"
)

// managementNetwork runs before routing, authentication and admission. The
// shared surface table includes console assets and unknown management paths,
// so neither a public auth endpoint nor the SPA bypasses the network policy.
// Private listeners do not use this handler. Unconfigured deployments retain
// the original handler, with no additional work on the inference hot path.
func managementNetwork(allowed, trusted []netip.Prefix, next http.Handler) http.Handler {
	if len(allowed) == 0 {
		return next
	}
	allowed, trusted = slices.Clone(allowed), slices.Clone(trusted)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if surface.Of(r.URL.Path).Inference {
			next.ServeHTTP(w, r)
			return
		}
		addr, err := netip.ParseAddr(consumerauth.ClientIP(r, trusted))
		if err == nil && addr.Zone() == "" && slices.ContainsFunc(allowed, func(prefix netip.Prefix) bool { return prefix.Contains(addr.Unmap()) }) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		problem := httpkit.Fail(http.StatusForbidden, "management_ip_not_allowed", "Management access is not allowed from this client address.")
		if strings.HasPrefix(r.URL.Path, "/scim/") {
			identity.WriteSCIMError(w, problem)
		} else {
			httpkit.WriteProblem(w, problem)
		}
	})
}
