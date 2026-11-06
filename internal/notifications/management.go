package notifications

import (
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"net/http"
)

type Server struct {
	access *access.Server
	egress *egress.Policy
}

func NewManagement(auth *access.Server, policy *egress.Policy) *Server {
	return &Server{access: auth, egress: policy}
}
func (s *Server) Register(mux *http.ServeMux) {
	for pattern, handler := range map[string]access.Handler{
		"GET /api/v1/notifications/destinations":                                  s.notificationDestinations,
		"POST /api/v1/notifications/destinations":                                 s.createNotificationDestination,
		"GET /api/v1/notifications/destinations/{notification_destination_id}":    s.notificationDestination,
		"PATCH /api/v1/notifications/destinations/{notification_destination_id}":  s.updateNotificationDestination,
		"DELETE /api/v1/notifications/destinations/{notification_destination_id}": s.deleteNotificationDestination,
		"GET /api/v1/notifications/rules":                                         s.notificationRules,
		"POST /api/v1/notifications/rules":                                        s.createNotificationRule,
		"GET /api/v1/notifications/rules/{notification_rule_id}":                  s.notificationRule,
		"PATCH /api/v1/notifications/rules/{notification_rule_id}":                s.updateNotificationRule,
		"DELETE /api/v1/notifications/rules/{notification_rule_id}":               s.deleteNotificationRule,
		"GET /api/v1/notifications/deliveries":                                    s.notificationDeliveries,
	} {
		s.access.Route(mux, pattern, handler)
	}
}
