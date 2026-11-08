package notifications

import (
	"net/http"

	"strings"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
)

const deliveryFields = `'api_key_id',v.api_key_id,'api_key_name',v.payload->'api_key_name','due_at',v.due_at,'reason',v.reason,'id',v.id,'rule_id',v.rule_id,'rule_name',r.name,'project_id',r.project_id,'event',CASE WHEN v.api_key_id IS NOT NULL THEN 'key.expiring' ELSE COALESCE(v.event,r.event) END,'dedup_key',v.dedup_key,'resolved',v.resolved,'evidence',v.payload,'window_id',v.window_id,'threshold_percent',v.threshold_percent,'accrued',v.accrued::text,'limit',v.limit_amount::text,'currency',v.currency,'provider_id',v.payload->'provider_id','provider_name',v.payload->'provider_name','credential_version_id',v.credential_id,'credential_version',v.payload->'credential_version','status',v.status,'attempts',v.attempts,'last_error_code',v.last_error_code,'created_at',v.created_at,'last_attempt_at',v.last_attempt_at,'delivered_at',v.delivered_at`

const deliveryFrom = ` FROM olp.notification_deliveries v
	JOIN olp.notification_rules r ON r.id=v.rule_id`

var listableStatuses = map[contract.NotificationDeliveryStatus]bool{
	contract.NotificationDeliveryStatusPending:   true,
	contract.NotificationDeliveryStatusDelivered: true,
	contract.NotificationDeliveryStatusFailed:    true,
}

func (s *Server) notificationDeliveries(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	query := r.URL.Query()
	var ruleID *string
	if raw := strings.TrimSpace(query.Get("rule_id")); raw != "" {
		parsed, err := access.ParseUUID(raw)
		if err != nil {
			return access.Reply{}, access.Invalid("rule_id", "Use a valid rule identifier.")
		}
		ruleID = &parsed
	}
	var status *contract.NotificationDeliveryStatus
	if raw := contract.NotificationDeliveryStatus(strings.TrimSpace(query.Get("status"))); raw != "" {
		if !listableStatuses[raw] {
			return access.Reply{}, access.Invalid("status", "Use pending, delivered, or failed.")
		}
		status = &raw
	}
	rows, err := s.access.Pool.Query(r.Context(),
		"SELECT jsonb_build_object("+deliveryFields+")"+deliveryFrom+
			" WHERE v.id<$1::uuid AND ($2 OR r.project_id=ANY($3::uuid[]))"+
			" AND ($4::uuid IS NULL OR v.rule_id=$4) AND ($5::text IS NULL OR v.status=$5)"+
			" ORDER BY v.id DESC LIMIT $6",
		page.Before, p.AllProjects, p.ProjectIDs(), ruleID, status, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	items, err := access.JSONRows(rows)
	return access.ListReply(items, page), err
}
