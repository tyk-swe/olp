package process

import (
	"net/http"
	"strings"

	"github.com/tyk-swe/olp/internal/surface"
)

// Perimeter sets the security headers of every public response before
// admission, routing, or any handler runs, so admission refusals, fallback
// 404s and console errors carry them as well. Handlers may still replace a
// default they own, such as the published contract's revalidating cache or
// the console's script policy.
//
// Browsers get HSTS whenever the public origin is HTTPS; OLP never serves TLS
// itself, so the edge that terminates it is what the header protects. The
// management API is framed nowhere and loads nothing. The console is framed
// nowhere, isolated from other windows, and denied device features it never
// uses. Inference responses stay cross-origin readable for browser SDKs
// through CORS, so they carry no resource policy.
func Perimeter(publicOrigin string, next http.Handler) http.Handler {
	hsts := strings.HasPrefix(publicOrigin, "https://")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		if hsts {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		switch {
		case surface.Of(r.URL.Path).Inference:
			h.Set("Cache-Control", "no-store")
		case strings.HasPrefix(r.URL.Path, "/api/"):
			h.Set("Cache-Control", "no-store")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Cross-Origin-Resource-Policy", "same-origin")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		default:
			h.Set("Referrer-Policy", "same-origin")
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			h.Set("Cross-Origin-Resource-Policy", "same-origin")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=(), serial=(), bluetooth=(), hid=()")
		}
		next.ServeHTTP(w, r)
	})
}
