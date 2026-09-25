package access

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

const notificationSecretPurpose = "notification_secret"

const destinationFields = `'id',d.id,'name',d.name,'url',d.url,'project_id',d.project_id,'project_name',p.name,'enabled',d.enabled,'etag',d.etag,'created_by',d.created_by,'created_by_email',u.email,'created_at',d.created_at,'updated_at',d.updated_at`
const destinationFrom = ` FROM olp_go.notification_destinations d
	JOIN olp_go.users u ON u.id=d.created_by LEFT JOIN olp_go.projects p ON p.id=d.project_id`

const ruleFields = `'id',r.id,'name',r.name,'project_id',r.project_id,'project_name',p.name,'subject_kind',r.subject_kind,'subject_id',r.subject_id,'subject_name',COALESCE(k.name,g.name),'window_kind',r.window_kind,'threshold_percent',r.threshold_percent,'destination_id',r.destination_id,'destination_name',d.name,'enabled',r.enabled,'etag',r.etag,'created_by',r.created_by,'created_by_email',u.email,'created_at',r.created_at,'updated_at',r.updated_at`
const ruleFrom = ` FROM olp_go.budget_alert_rules r
	JOIN olp_go.users u ON u.id=r.created_by
	LEFT JOIN olp_go.projects p ON p.id=r.project_id
	LEFT JOIN olp_go.api_keys k ON r.subject_kind='api_key' AND k.id=r.subject_id
	LEFT JOIN olp_go.budget_groups g ON r.subject_kind='budget_group' AND g.id=r.subject_id
	JOIN olp_go.notification_destinations d ON d.id=r.destination_id`

const deliveryFields = `'id',v.id,'rule_id',v.rule_id,'rule_name',r.name,'project_id',r.project_id,'window_id',v.window_id,'threshold_percent',v.threshold_percent,'accrued',v.accrued::text,'limit',v.limit_amount::text,'currency',v.currency,'status',v.status,'attempts',v.attempts,'last_error_code',v.last_error_code,'created_at',v.created_at,'last_attempt_at',v.last_attempt_at,'delivered_at',v.delivered_at`
const deliveryFrom = ` FROM olp_go.budget_alert_deliveries v
	JOIN olp_go.budget_alert_rules r ON r.id=v.rule_id`

type destinationInput struct {
	Name      string           `json:"name"`
	URL       string           `json:"url"`
	ProjectID *string          `json:"project_id"`
	Secret    *json.RawMessage `json:"secret"`
	Enabled   *bool            `json:"enabled"`
}

type ruleInput struct {
	Name             string  `json:"name"`
	ProjectID        *string `json:"project_id"`
	SubjectKind      string  `json:"subject_kind"`
	SubjectID        string  `json:"subject_id"`
	WindowKind       string  `json:"window_kind"`
	ThresholdPercent *int    `json:"threshold_percent"`
	DestinationID    string  `json:"destination_id"`
	Enabled          *bool   `json:"enabled"`
}

func notificationPermission(projectID *string) string {
	if projectID == nil {
		return "settings"
	}
	return "keys"
}

func (s *Server) notificationDestinations(r *http.Request) (Reply, error) {
	p, err := s.Principal(r, s.Pool, "read")
	if err != nil {
		return Reply{}, err
	}
	page, err := Page(r)
	if err != nil {
		return Reply{}, err
	}
	rows, err := s.Pool.Query(r.Context(),
		"SELECT jsonb_build_object("+destinationFields+")"+destinationFrom+
			" WHERE d.id<$1::uuid AND ($2 OR d.project_id=ANY($3::uuid[])) ORDER BY d.id DESC LIMIT $4",
		page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return Reply{}, err
	}
	items, err := JSONRows(rows)
	return ListReply(items, page), err
}

func (s *Server) notificationDestination(r *http.Request) (Reply, error) {
	p, err := s.Principal(r, s.Pool, "read")
	if err != nil {
		return Reply{}, err
	}
	id, err := IDParam(r, "notification_destination_id")
	if err != nil {
		return Reply{}, err
	}
	var data []byte
	var etag string
	var projectID *string
	if err = s.Pool.QueryRow(r.Context(),
		"SELECT jsonb_build_object("+destinationFields+"),d.etag::text,d.project_id::text"+destinationFrom+" WHERE d.id=$1",
		id).Scan(&data, &etag, &projectID); err != nil {
		return Reply{}, err
	}
	if !p.CanProject(projectID, false) {
		return Reply{}, pgx.ErrNoRows
	}
	return Detail(json.RawMessage(data), etag), nil
}

func (s *Server) validateDestinationURL(raw string) error {
	if s.Egress == nil {
		return Fail(503, "egress_policy_unavailable", "Notification delivery is not configured on this installation.")
	}
	if _, err := s.Egress.ValidateEndpoint(raw); err != nil {
		return Fail(422, "invalid_url", "The destination URL is not permitted by egress policy.")
	}
	return nil
}

func (s *Server) storeNotificationSecret(r *http.Request, tx pgx.Tx, id string, raw *json.RawMessage) error {
	if raw == nil {
		return nil
	}
	var secret *string
	if err := json.Unmarshal(*raw, &secret); err != nil {
		return Invalid("secret", "Use a signing secret string or null.")
	}
	if secret != nil && (*secret == "" || len(*secret) > 1024) {
		return Invalid("secret", "Use a signing secret of 1–1024 bytes.")
	}
	// The destination solely owns its signing secret. Replacing or clearing it
	// deletes the previous record, whose foreign key clears secret_id.
	if _, err := tx.Exec(r.Context(),
		"DELETE FROM olp_go.secrets WHERE id=(SELECT secret_id FROM olp_go.notification_destinations WHERE id=$1) AND purpose=$2",
		id, notificationSecretPurpose); err != nil {
		return err
	}
	if secret == nil {
		return nil
	}
	secretID := NewID()
	if err := s.Keys.Store(r.Context(), tx, s.Installation, secretID, notificationSecretPurpose, []byte(*secret), nil); err != nil {
		return err
	}
	_, err := tx.Exec(r.Context(),
		"UPDATE olp_go.notification_destinations SET secret_id=$2 WHERE id=$1", id, secretID)
	return err
}

func (s *Server) validateDestination(input destinationInput) error {
	if err := ValidText("name", input.Name, 100); err != nil {
		return err
	}
	return s.validateDestinationURL(input.URL)
}

func (s *Server) createNotificationDestination(r *http.Request) (Reply, error) {
	var input destinationInput
	if err := Decode(r, &input); err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Principal(r, tx, notificationPermission(input.ProjectID))
	if err != nil {
		return Reply{}, err
	}
	claim, replayed, err := s.Replay(r, tx, p, input)
	if err != nil {
		return Reply{}, err
	}
	if replayed != nil {
		return Commit(r, tx, *replayed)
	}
	if err = s.validateDestination(input); err != nil {
		return Reply{}, err
	}
	if input.ProjectID != nil {
		var parsed string
		if parsed, err = ParseUUID(*input.ProjectID); err != nil {
			return Reply{}, err
		}
		input.ProjectID = &parsed
	}
	if err = s.RequireProject(r.Context(), tx, p, input.ProjectID, true); err != nil {
		return Reply{}, err
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	id, etag := NewID(), NewID()
	if _, err = tx.Exec(r.Context(),
		`INSERT INTO olp_go.notification_destinations (id, name, url, project_id, etag, created_by, enabled)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		id, strings.TrimSpace(input.Name), strings.TrimSpace(input.URL), input.ProjectID, etag, p.UserID(), enabled); err != nil {
		return Reply{}, err
	}
	if err = s.storeNotificationSecret(r, tx, id, input.Secret); err != nil {
		return Reply{}, err
	}
	var data []byte
	if err = tx.QueryRow(r.Context(),
		"SELECT jsonb_build_object("+destinationFields+")"+destinationFrom+" WHERE d.id=$1", id).Scan(&data); err != nil {
		return Reply{}, err
	}
	result := Reply{Status: 201, ETag: etag, Location: "/api/v3/notifications/destinations/" + id,
		Body: json.RawMessage(data)}
	if err = Audit(r.Context(), tx, r, p.ID, "notification_destination.create",
		"notification_destination", id, "success"); err != nil {
		return Reply{}, err
	}
	if err = s.CompleteReplay(r, tx, claim, result); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, result)
}

func (s *Server) updateNotificationDestination(r *http.Request) (Reply, error) {
	var patch map[string]json.RawMessage
	if err := Decode(r, &patch); err != nil {
		return Reply{}, err
	}
	if len(patch) == 0 {
		return Reply{}, Invalid("destination", "Send at least one field to update.")
	}
	for field := range patch {
		if field != "name" && field != "url" && field != "enabled" && field != "secret" {
			return Reply{}, Invalid(field, "Unknown destination field.")
		}
	}
	id, err := IDParam(r, "notification_destination_id")
	if err != nil {
		return Reply{}, err
	}
	var projectID *string
	if err = s.Pool.QueryRow(r.Context(),
		"SELECT project_id::text FROM olp_go.notification_destinations WHERE id=$1", id).Scan(&projectID); err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Principal(r, tx, notificationPermission(projectID))
	if err != nil {
		return Reply{}, err
	}
	if err = ProjectAccess(p, projectID, true); err != nil {
		return Reply{}, err
	}
	var data []byte
	var etag string
	if err = tx.QueryRow(r.Context(),
		"SELECT jsonb_build_object("+destinationFields+"),d.etag::text"+destinationFrom+" WHERE d.id=$1 FOR UPDATE OF d",
		id).Scan(&data, &etag); err != nil {
		return Reply{}, err
	}
	if err = Match(r, etag); err != nil {
		return Reply{}, err
	}
	var current struct {
		Name    string `json:"name"`
		URL     string `json:"url"`
		Enabled bool   `json:"enabled"`
	}
	if err = json.Unmarshal(data, &current); err != nil {
		return Reply{}, err
	}
	if raw, ok := patch["name"]; ok {
		if err = json.Unmarshal(raw, &current.Name); err != nil {
			return Reply{}, Invalid("name", "Use a non-empty destination name.")
		}
		if err = ValidText("name", current.Name, 100); err != nil {
			return Reply{}, err
		}
	}
	if raw, ok := patch["url"]; ok {
		if err = json.Unmarshal(raw, &current.URL); err != nil {
			return Reply{}, Invalid("url", "Use a valid destination URL.")
		}
		if err = s.validateDestinationURL(current.URL); err != nil {
			return Reply{}, err
		}
	}
	if raw, ok := patch["enabled"]; ok {
		if err = json.Unmarshal(raw, &current.Enabled); err != nil {
			return Reply{}, Invalid("enabled", "Use true or false.")
		}
	}
	etag = NewID()
	if _, err = tx.Exec(r.Context(),
		"UPDATE olp_go.notification_destinations SET name=$2,url=$3,enabled=$4,etag=$5,updated_at=now() WHERE id=$1",
		id, strings.TrimSpace(current.Name), strings.TrimSpace(current.URL), current.Enabled, etag); err != nil {
		return Reply{}, err
	}
	if raw, ok := patch["secret"]; ok {
		if err = s.storeNotificationSecret(r, tx, id, &raw); err != nil {
			return Reply{}, err
		}
	}
	if err = tx.QueryRow(r.Context(),
		"SELECT jsonb_build_object("+destinationFields+")"+destinationFrom+" WHERE d.id=$1", id).Scan(&data); err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.ID, "notification_destination.update",
		"notification_destination", id, "success"); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, Detail(json.RawMessage(data), etag))
}

func (s *Server) notificationRules(r *http.Request) (Reply, error) {
	p, err := s.Principal(r, s.Pool, "read")
	if err != nil {
		return Reply{}, err
	}
	page, err := Page(r)
	if err != nil {
		return Reply{}, err
	}
	rows, err := s.Pool.Query(r.Context(),
		"SELECT jsonb_build_object("+ruleFields+")"+ruleFrom+
			" WHERE r.id<$1::uuid AND ($2 OR r.project_id=ANY($3::uuid[])) ORDER BY r.id DESC LIMIT $4",
		page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return Reply{}, err
	}
	items, err := JSONRows(rows)
	return ListReply(items, page), err
}

func (s *Server) notificationRule(r *http.Request) (Reply, error) {
	p, err := s.Principal(r, s.Pool, "read")
	if err != nil {
		return Reply{}, err
	}
	id, err := IDParam(r, "budget_alert_rule_id")
	if err != nil {
		return Reply{}, err
	}
	var data []byte
	var etag string
	var projectID *string
	if err = s.Pool.QueryRow(r.Context(),
		"SELECT jsonb_build_object("+ruleFields+"),r.etag::text,r.project_id::text"+ruleFrom+" WHERE r.id=$1",
		id).Scan(&data, &etag, &projectID); err != nil {
		return Reply{}, err
	}
	if !p.CanProject(projectID, false) {
		return Reply{}, pgx.ErrNoRows
	}
	return Detail(json.RawMessage(data), etag), nil
}

func (s *Server) validateRuleSubject(r *http.Request, tx pgx.Tx, input ruleInput) error {
	if input.SubjectKind != "api_key" && input.SubjectKind != "budget_group" {
		return Invalid("subject_kind", "Use api_key or budget_group.")
	}
	if input.WindowKind != "day" && input.WindowKind != "month" {
		return Invalid("window_kind", "Use day or month.")
	}
	if input.ThresholdPercent == nil || *input.ThresholdPercent < 1 || *input.ThresholdPercent > 100 {
		return Invalid("threshold_percent", "Use a threshold from 1 to 100.")
	}
	subjectID, err := ParseUUID(input.SubjectID)
	if err != nil {
		return Invalid("subject_id", "Use a valid subject identifier.")
	}
	var subjectProject *string
	switch input.SubjectKind {
	case "api_key":
		err = tx.QueryRow(r.Context(),
			"SELECT project_id::text FROM olp_go.api_keys WHERE id=$1", subjectID).Scan(&subjectProject)
	case "budget_group":
		err = tx.QueryRow(r.Context(),
			"SELECT project_id::text FROM olp_go.budget_groups WHERE id=$1", subjectID).Scan(&subjectProject)
	}
	if err != nil {
		if err == pgx.ErrNoRows {
			return Invalid("subject_id", "The alert subject does not exist.")
		}
		return err
	}
	if (subjectProject == nil) != (input.ProjectID == nil) ||
		(subjectProject != nil && *subjectProject != *input.ProjectID) {
		return Invalid("subject_id", "The alert subject must belong to the rule's project.")
	}
	return nil
}

func (s *Server) validateRuleDestination(r *http.Request, tx pgx.Tx, input ruleInput) error {
	destinationID, err := ParseUUID(input.DestinationID)
	if err != nil {
		return Invalid("destination_id", "Use a valid destination identifier.")
	}
	var destinationProject *string
	if err = tx.QueryRow(r.Context(),
		"SELECT project_id::text FROM olp_go.notification_destinations WHERE id=$1", destinationID).Scan(&destinationProject); err != nil {
		if err == pgx.ErrNoRows {
			return Invalid("destination_id", "The notification destination does not exist.")
		}
		return err
	}
	if (destinationProject == nil) != (input.ProjectID == nil) ||
		(destinationProject != nil && *destinationProject != *input.ProjectID) {
		return Invalid("destination_id", "The notification destination must belong to the rule's project.")
	}
	return nil
}

func (s *Server) validateRule(r *http.Request, tx pgx.Tx, input ruleInput) error {
	if err := ValidText("name", input.Name, 100); err != nil {
		return err
	}
	if err := s.validateRuleSubject(r, tx, input); err != nil {
		return err
	}
	return s.validateRuleDestination(r, tx, input)
}

func (s *Server) createNotificationRule(r *http.Request) (Reply, error) {
	var input ruleInput
	if err := Decode(r, &input); err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Principal(r, tx, notificationPermission(input.ProjectID))
	if err != nil {
		return Reply{}, err
	}
	claim, replayed, err := s.Replay(r, tx, p, input)
	if err != nil {
		return Reply{}, err
	}
	if replayed != nil {
		return Commit(r, tx, *replayed)
	}
	if input.ProjectID != nil {
		var parsed string
		if parsed, err = ParseUUID(*input.ProjectID); err != nil {
			return Reply{}, err
		}
		input.ProjectID = &parsed
	}
	if err = s.RequireProject(r.Context(), tx, p, input.ProjectID, true); err != nil {
		return Reply{}, err
	}
	if err = s.validateRule(r, tx, input); err != nil {
		return Reply{}, err
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	id, etag := NewID(), NewID()
	subjectID, _ := ParseUUID(input.SubjectID)
	destinationID, _ := ParseUUID(input.DestinationID)
	if _, err = tx.Exec(r.Context(),
		`INSERT INTO olp_go.budget_alert_rules
		 (id, name, project_id, subject_kind, subject_id, window_kind, threshold_percent,
		  destination_id, enabled, etag, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		id, strings.TrimSpace(input.Name), input.ProjectID, input.SubjectKind, subjectID,
		input.WindowKind, *input.ThresholdPercent, destinationID, enabled, etag, p.UserID()); err != nil {
		return Reply{}, err
	}
	var data []byte
	if err = tx.QueryRow(r.Context(),
		"SELECT jsonb_build_object("+ruleFields+")"+ruleFrom+" WHERE r.id=$1", id).Scan(&data); err != nil {
		return Reply{}, err
	}
	result := Reply{Status: 201, ETag: etag, Location: "/api/v3/notifications/rules/" + id,
		Body: json.RawMessage(data)}
	if err = Audit(r.Context(), tx, r, p.ID, "budget_alert_rule.create",
		"budget_alert_rule", id, "success"); err != nil {
		return Reply{}, err
	}
	if err = s.CompleteReplay(r, tx, claim, result); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, result)
}

func (s *Server) updateNotificationRule(r *http.Request) (Reply, error) {
	var patch map[string]json.RawMessage
	if err := Decode(r, &patch); err != nil {
		return Reply{}, err
	}
	if len(patch) == 0 {
		return Reply{}, Invalid("rule", "Send at least one field to update.")
	}
	allowed := []string{"name", "subject_kind", "subject_id", "window_kind",
		"threshold_percent", "destination_id", "enabled"}
	for field := range patch {
		if !slices.Contains(allowed, field) {
			return Reply{}, Invalid(field, "Unknown alert rule field.")
		}
	}
	id, err := IDParam(r, "budget_alert_rule_id")
	if err != nil {
		return Reply{}, err
	}
	var projectID *string
	if err = s.Pool.QueryRow(r.Context(),
		"SELECT project_id::text FROM olp_go.budget_alert_rules WHERE id=$1", id).Scan(&projectID); err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Principal(r, tx, notificationPermission(projectID))
	if err != nil {
		return Reply{}, err
	}
	if err = ProjectAccess(p, projectID, true); err != nil {
		return Reply{}, err
	}
	var data []byte
	var etag string
	if err = tx.QueryRow(r.Context(),
		"SELECT jsonb_build_object("+ruleFields+"),r.etag::text"+ruleFrom+" WHERE r.id=$1 FOR UPDATE OF r",
		id).Scan(&data, &etag); err != nil {
		return Reply{}, err
	}
	if err = Match(r, etag); err != nil {
		return Reply{}, err
	}
	var next ruleInput
	if err = json.Unmarshal(data, &next); err != nil {
		return Reply{}, err
	}
	for field, raw := range patch {
		switch field {
		case "name":
			if err = json.Unmarshal(raw, &next.Name); err != nil {
				return Reply{}, Invalid("name", "Use a non-empty rule name.")
			}
		case "subject_kind":
			if err = json.Unmarshal(raw, &next.SubjectKind); err != nil {
				return Reply{}, Invalid("subject_kind", "Use api_key or budget_group.")
			}
		case "subject_id":
			if err = json.Unmarshal(raw, &next.SubjectID); err != nil {
				return Reply{}, Invalid("subject_id", "Use a valid subject identifier.")
			}
		case "window_kind":
			if err = json.Unmarshal(raw, &next.WindowKind); err != nil {
				return Reply{}, Invalid("window_kind", "Use day or month.")
			}
		case "threshold_percent":
			if err = json.Unmarshal(raw, &next.ThresholdPercent); err != nil {
				return Reply{}, Invalid("threshold_percent", "Use a threshold from 1 to 100.")
			}
		case "destination_id":
			if err = json.Unmarshal(raw, &next.DestinationID); err != nil {
				return Reply{}, Invalid("destination_id", "Use a valid destination identifier.")
			}
		case "enabled":
			if err = json.Unmarshal(raw, &next.Enabled); err != nil {
				return Reply{}, Invalid("enabled", "Use true or false.")
			}
		}
	}
	next.ProjectID = projectID
	if err = s.validateRule(r, tx, next); err != nil {
		return Reply{}, err
	}
	subjectID, _ := ParseUUID(next.SubjectID)
	destinationID, _ := ParseUUID(next.DestinationID)
	enabled := next.Enabled != nil && *next.Enabled
	etag = NewID()
	if _, err = tx.Exec(r.Context(),
		`UPDATE olp_go.budget_alert_rules SET name=$2,subject_kind=$3,subject_id=$4,window_kind=$5,
		 threshold_percent=$6,destination_id=$7,enabled=$8,etag=$9,updated_at=now() WHERE id=$1`,
		id, strings.TrimSpace(next.Name), next.SubjectKind, subjectID, next.WindowKind,
		*next.ThresholdPercent, destinationID, enabled, etag); err != nil {
		return Reply{}, err
	}
	if err = tx.QueryRow(r.Context(),
		"SELECT jsonb_build_object("+ruleFields+")"+ruleFrom+" WHERE r.id=$1", id).Scan(&data); err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.ID, "budget_alert_rule.update",
		"budget_alert_rule", id, "success"); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, Detail(json.RawMessage(data), etag))
}

func (s *Server) notificationDeliveries(r *http.Request) (Reply, error) {
	p, err := s.Principal(r, s.Pool, "read")
	if err != nil {
		return Reply{}, err
	}
	page, err := Page(r)
	if err != nil {
		return Reply{}, err
	}
	query := r.URL.Query()
	var ruleID *string
	if raw := strings.TrimSpace(query.Get("rule_id")); raw != "" {
		parsed, err := ParseUUID(raw)
		if err != nil {
			return Reply{}, Invalid("rule_id", "Use a valid rule identifier.")
		}
		ruleID = &parsed
	}
	var status *string
	if raw := strings.TrimSpace(query.Get("status")); raw != "" {
		if raw != "pending" && raw != "delivered" && raw != "failed" {
			return Reply{}, Invalid("status", "Use pending, delivered, or failed.")
		}
		status = &raw
	}
	rows, err := s.Pool.Query(r.Context(),
		"SELECT jsonb_build_object("+deliveryFields+")"+deliveryFrom+
			" WHERE v.id<$1::uuid AND ($2 OR r.project_id=ANY($3::uuid[]))"+
			" AND ($4::uuid IS NULL OR v.rule_id=$4) AND ($5::text IS NULL OR v.status=$5)"+
			" ORDER BY v.id DESC LIMIT $6",
		page.Before, p.AllProjects, p.ProjectIDs(), ruleID, status, page.Limit+1)
	if err != nil {
		return Reply{}, err
	}
	items, err := JSONRows(rows)
	return ListReply(items, page), err
}
