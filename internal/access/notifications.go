package access

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/notifications"
	"github.com/tyk-swe/olp/internal/secrets"
)

// The events a notification rule subscribes its destination to. A budget
// threshold concerns an API key's or budget group's spend; a provider event,
// such as a grant lapse, concerns the whole installation.
const (
	BudgetThresholdEvent = "budget.threshold"
	KeyExpiringEvent     = "key.expiring"
	GrantLapsedEvent     = "provider.grant.lapsed"
)

const destinationFields = `'id',d.id,'name',d.name,'url',d.url,'type',d.type,'configuration',d.configuration,'secret_configured',d.secret_id IS NOT NULL,'project_id',d.project_id,'project_name',p.name,'enabled',d.enabled,'etag',d.etag,'created_by',d.created_by,'created_by_email',u.email,'created_at',d.created_at,'updated_at',d.updated_at`
const destinationFrom = ` FROM olp.notification_destinations d
	JOIN olp.users u ON u.id=d.created_by LEFT JOIN olp.projects p ON p.id=d.project_id`

const ruleFields = `'id',r.id,'name',r.name,'project_id',r.project_id,'project_name',p.name,'event',r.event,'configuration',r.configuration,'subject_kind',r.subject_kind,'subject_id',r.subject_id,'subject_name',COALESCE(k.name,g.name),'window_kind',r.window_kind,'threshold_percent',r.threshold_percent,'destination_id',r.destination_id,'destination_name',d.name,'enabled',r.enabled,'etag',r.etag,'created_by',r.created_by,'created_by_email',u.email,'created_at',r.created_at,'updated_at',r.updated_at`
const ruleFrom = ` FROM olp.notification_rules r
	JOIN olp.users u ON u.id=r.created_by
	LEFT JOIN olp.projects p ON p.id=r.project_id
	LEFT JOIN olp.api_keys k ON r.subject_kind='api_key' AND k.id=r.subject_id
	LEFT JOIN olp.budget_groups g ON r.subject_kind='budget_group' AND g.id=r.subject_id
	JOIN olp.notification_destinations d ON d.id=r.destination_id`

const deliveryFields = `'api_key_id',v.api_key_id,'api_key_name',v.payload->'api_key_name','due_at',v.due_at,'reason',v.reason,'id',v.id,'rule_id',v.rule_id,'rule_name',r.name,'project_id',r.project_id,'event',CASE WHEN v.api_key_id IS NOT NULL THEN 'key.expiring' ELSE COALESCE(v.event,r.event) END,'dedup_key',v.dedup_key,'resolved',v.resolved,'evidence',v.payload,'window_id',v.window_id,'threshold_percent',v.threshold_percent,'accrued',v.accrued::text,'limit',v.limit_amount::text,'currency',v.currency,'provider_id',v.payload->'provider_id','provider_name',v.payload->'provider_name','credential_version_id',v.credential_id,'credential_version',v.payload->'credential_version','status',v.status,'attempts',v.attempts,'last_error_code',v.last_error_code,'created_at',v.created_at,'last_attempt_at',v.last_attempt_at,'delivered_at',v.delivered_at`
const deliveryFrom = ` FROM olp.notification_deliveries v
	JOIN olp.notification_rules r ON r.id=v.rule_id`

type destinationInput struct {
	Name          string           `json:"name"`
	URL           string           `json:"url"`
	Type          string           `json:"type"`
	Configuration json.RawMessage  `json:"configuration"`
	ProjectID     *string          `json:"project_id"`
	Secret        *json.RawMessage `json:"secret"`
	Enabled       *bool            `json:"enabled"`
}

// ruleInput is a notification rule as written. Only a budget threshold rule
// has a subject, window and threshold.
type ruleInput struct {
	Name             string          `json:"name"`
	ProjectID        *string         `json:"project_id"`
	Event            string          `json:"event"`
	SubjectKind      *string         `json:"subject_kind"`
	SubjectID        *string         `json:"subject_id"`
	WindowKind       *string         `json:"window_kind"`
	ThresholdPercent *int            `json:"threshold_percent"`
	DestinationID    string          `json:"destination_id"`
	Enabled          *bool           `json:"enabled"`
	Configuration    json.RawMessage `json:"configuration"`
}

// notificationOperation is what writing a notification destination or rule
// requires: installation-wide ones are installation settings, and project
// ones are managed with the project's keys.
func notificationOperation(projectID *string) Operation {
	if projectID == nil {
		return Settings
	}
	return Keys
}

func (s *Server) notificationDestinations(r *http.Request, p Principal) (Reply, error) {
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

func (s *Server) notificationDestination(r *http.Request, p Principal) (Reply, error) {
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
	if err := p.Project(projectID, View); err != nil {
		return Reply{}, err
	}
	return Detail(json.RawMessage(data), etag), nil
}

func (s *Server) storeNotificationSecret(r *http.Request, tx pgx.Tx, id, typ string, raw *json.RawMessage) error {
	if raw == nil {
		return nil
	}
	var secretID *string
	if string(*raw) != "null" {
		var secret []byte
		if typ == "webhook" {
			var value string
			if err := json.Unmarshal(*raw, &value); err != nil {
				return Invalid("secret", "Use a signing secret string or null.")
			}
			if value == "" || len(value) > 1024 {
				return Invalid("secret", "Use a signing secret of 1–1024 bytes.")
			}
			secret = []byte(value)
		} else {
			if len(*raw) > 16384 {
				return Invalid("secret", "The secret object is too large.")
			}
			var decoded map[string]json.RawMessage
			if err := json.Unmarshal(*raw, &decoded); err != nil || decoded == nil {
				return Invalid("secret", "Use the channel credential object or null.")
			}
			secret = *raw
		}
		stored := NewID()
		if err := s.Keys.Store(r.Context(), tx, s.Installation, stored, secrets.NotificationSecret, secret, nil); err != nil {
			return err
		}
		secretID = &stored
	}
	// Callers hold the destination row, so the replaced secret can be deleted
	// once nothing references it; a rotated or cleared secret must not linger.
	var previous *string
	if err := tx.QueryRow(r.Context(),
		"SELECT secret_id::text FROM olp.notification_destinations WHERE id=$1", id).Scan(&previous); err != nil {
		return err
	}
	if _, err := tx.Exec(r.Context(),
		"UPDATE olp.notification_destinations SET secret_id=$2 WHERE id=$1", id, secretID); err != nil {
		return err
	}
	if previous == nil {
		return nil
	}
	_, err := tx.Exec(r.Context(), "DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2", *previous, secrets.NotificationSecret)
	return err
}

func (s *Server) validateDestination(input destinationInput) error {
	if err := ValidText("name", input.Name, 100); err != nil {
		return err
	}
	if s.Egress == nil {
		return Fail(503, "egress_policy_unavailable", "Notification delivery is not configured on this installation.")
	}
	if len(input.Configuration) > 4096 {
		return Invalid("configuration", "Use a configuration object of at most 4096 bytes.")
	}
	secret, err := destinationSecret(input)
	if err != nil {
		return err
	}
	d := notifications.Destination{Type: input.Type, URL: strings.TrimSpace(input.URL), Configuration: input.Configuration}
	if secret == nil {
		if err := notifications.ValidateDestinationShape(s.Egress, d); err != nil {
			return Invalid("destination", "The destination or its configuration is invalid.")
		}
		if notifications.RequiresSecret(d.Type) && (input.Enabled == nil || *input.Enabled) {
			return Invalid("secret", "This destination type requires a credential secret object.")
		}
		return nil
	}
	if err := notifications.ValidateDestination(s.Egress, d, secret); err != nil {
		return Invalid("destination", "The destination or its secret is invalid.")
	}
	return nil
}

func destinationSecret(input destinationInput) ([]byte, error) {
	if input.Secret == nil || string(*input.Secret) == "null" {
		return nil, nil
	}
	if input.Type == "" || input.Type == "webhook" {
		var secret string
		if err := json.Unmarshal(*input.Secret, &secret); err != nil {
			return nil, Invalid("secret", "Use a signing secret string or null.")
		}
		if secret == "" || len(secret) > 1024 {
			return nil, Invalid("secret", "Use a signing secret of 1–1024 bytes.")
		}
		return []byte(secret), nil
	}
	if len(*input.Secret) > 16384 {
		return nil, Invalid("secret", "The secret object is too large.")
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(*input.Secret, &decoded); err != nil || decoded == nil {
		return nil, Invalid("secret", "Use the channel credential object or null.")
	}
	return *input.Secret, nil
}

func (s *Server) createNotificationDestination(r *http.Request, _ Principal) (Reply, error) {
	var input destinationInput
	if err := Decode(r, &input); err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Reauthorize(r, tx)
	if err != nil {
		return Reply{}, err
	}
	if err = p.Authorize(notificationOperation(input.ProjectID)); err != nil {
		return Reply{}, err
	}
	claim, replayed, err := s.Replay(r, tx, p, input)
	if err != nil {
		return Reply{}, err
	}
	if replayed != nil {
		// The stored reply carries the created destination, so the caller
		// must still reach its project to receive it.
		if err := s.RequireProject(r.Context(), tx, p, input.ProjectID); err != nil {
			return Reply{}, err
		}
		return Commit(r, tx, *replayed)
	}
	if input.Type == "" {
		input.Type = "webhook"
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
	if err = s.RequireProject(r.Context(), tx, p, input.ProjectID); err != nil {
		return Reply{}, err
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	configuration := input.Configuration
	if len(configuration) == 0 || string(configuration) == "null" {
		configuration = json.RawMessage(`{}`)
	}
	id, etag := NewID(), NewID()
	if _, err = tx.Exec(r.Context(), createChannelDestinationSQL,
		id, strings.TrimSpace(input.Name), strings.TrimSpace(input.URL), input.ProjectID, etag,
		p.UserID(), enabled, input.Type, configuration); err != nil {
		return Reply{}, err
	}
	if err = s.storeNotificationSecret(r, tx, id, input.Type, input.Secret); err != nil {
		return Reply{}, err
	}
	var data []byte
	if err = tx.QueryRow(r.Context(),
		"SELECT jsonb_build_object("+destinationFields+")"+destinationFrom+" WHERE d.id=$1", id).Scan(&data); err != nil {
		return Reply{}, err
	}
	result := Reply{Status: 201, ETag: etag, Location: "/api/v1/notifications/destinations/" + id,
		Body: json.RawMessage(data)}
	if err = Audit(r.Context(), tx, r, p.Actor(), "notification_destination.create",
		"notification_destination", id, "success"); err != nil {
		return Reply{}, err
	}
	if err = s.CompleteReplay(r, tx, claim, result); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, result)
}

func (s *Server) updateNotificationDestination(r *http.Request, _ Principal) (Reply, error) {
	var patch map[string]json.RawMessage
	if err := Decode(r, &patch); err != nil {
		return Reply{}, err
	}
	if len(patch) == 0 {
		return Reply{}, Invalid("destination", "Send at least one field to update.")
	}
	for field := range patch {
		if field != "name" && field != "url" && field != "type" && field != "configuration" && field != "enabled" && field != "secret" {
			return Reply{}, Invalid(field, "Unknown destination field.")
		}
	}
	id, err := IDParam(r, "notification_destination_id")
	if err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Reauthorize(r, tx)
	if err != nil {
		return Reply{}, err
	}
	var projectID *string
	if err = tx.QueryRow(r.Context(),
		"SELECT project_id::text FROM olp.notification_destinations WHERE id=$1", id).Scan(&projectID); err != nil {
		return Reply{}, err
	}
	if err = p.Project(projectID, View); err != nil {
		return Reply{}, err
	}
	if err = p.Authorize(notificationOperation(projectID)); err != nil {
		return Reply{}, err
	}
	if err = p.Project(projectID, Change); err != nil {
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
		Name             string          `json:"name"`
		URL              string          `json:"url"`
		Type             string          `json:"type"`
		Configuration    json.RawMessage `json:"configuration"`
		Enabled          bool            `json:"enabled"`
		SecretConfigured bool            `json:"secret_configured"`
	}
	if err = json.Unmarshal(data, &current); err != nil {
		return Reply{}, err
	}
	previousURL := current.URL
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
	}
	if raw, ok := patch["type"]; ok {
		var patched string
		if err = json.Unmarshal(raw, &patched); err != nil || patched != current.Type {
			return Reply{}, Invalid("type", "The destination type cannot be changed.")
		}
	}
	if raw, ok := patch["configuration"]; ok {
		if string(raw) == "null" {
			current.Configuration = json.RawMessage(`{}`)
		} else {
			current.Configuration = json.RawMessage(raw)
		}
		if len(current.Configuration) > 4096 {
			return Reply{}, Invalid("configuration", "Use a configuration object of at most 4096 bytes.")
		}
		var decoded map[string]json.RawMessage
		if err = json.Unmarshal(current.Configuration, &decoded); err != nil || decoded == nil {
			return Reply{}, Invalid("configuration", "Use a configuration object.")
		}
	}
	if raw, ok := patch["enabled"]; ok {
		if err = json.Unmarshal(raw, &current.Enabled); err != nil {
			return Reply{}, Invalid("enabled", "Use true or false.")
		}
	}
	if s.Egress == nil {
		return Reply{}, Fail(503, "egress_policy_unavailable", "Notification delivery is not configured on this installation.")
	}
	d := notifications.Destination{Type: current.Type, URL: strings.TrimSpace(current.URL), Configuration: current.Configuration}
	if _, supplied := patch["secret"]; !supplied && current.SecretConfigured && d.URL != previousURL {
		return Reply{}, Invalid("secret", "Changing the destination URL requires a replacement secret or null.")
	}
	var effectiveSecret []byte
	secretCleared := false
	if raw, ok := patch["secret"]; ok {
		if string(raw) == "null" {
			secretCleared = true
		} else if current.Type == "webhook" {
			var value string
			if err = json.Unmarshal(raw, &value); err != nil {
				return Reply{}, Invalid("secret", "Use a signing secret string or null.")
			}
			effectiveSecret = []byte(value)
		} else {
			effectiveSecret = []byte(raw)
		}
	} else if current.SecretConfigured {
		var secretID *string
		if err = tx.QueryRow(r.Context(), readDestinationSecretSQL, id).Scan(&secretID); err != nil {
			return Reply{}, err
		}
		if secretID != nil {
			if effectiveSecret, err = s.Keys.Read(r.Context(), tx, s.Installation, *secretID, secrets.NotificationSecret); err != nil {
				return Reply{}, err
			}
		}
	}
	if len(effectiveSecret) != 0 || (!notifications.RequiresSecret(current.Type) && !secretCleared) {
		if err = notifications.ValidateDestination(s.Egress, d, effectiveSecret); err != nil {
			return Reply{}, Invalid("destination", "The destination, configuration or secret is invalid.")
		}
	} else {
		if err = notifications.ValidateDestinationShape(s.Egress, d); err != nil {
			return Reply{}, Invalid("destination", "The destination or its configuration is invalid.")
		}
		if current.Enabled && notifications.RequiresSecret(current.Type) {
			return Reply{}, Invalid("secret", "This destination type requires a credential secret object.")
		}
	}
	etag = NewID()
	if _, err = tx.Exec(r.Context(), updateChannelDestinationSQL,
		id, strings.TrimSpace(current.Name), strings.TrimSpace(current.URL), current.Enabled, etag,
		current.Type, current.Configuration); err != nil {
		return Reply{}, err
	}
	if raw, ok := patch["secret"]; ok {
		if err = s.storeNotificationSecret(r, tx, id, current.Type, &raw); err != nil {
			return Reply{}, err
		}
	}
	if err = tx.QueryRow(r.Context(),
		"SELECT jsonb_build_object("+destinationFields+")"+destinationFrom+" WHERE d.id=$1", id).Scan(&data); err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.Actor(), "notification_destination.update",
		"notification_destination", id, "success"); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, Detail(json.RawMessage(data), etag))
}

func (s *Server) notificationRules(r *http.Request, p Principal) (Reply, error) {
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

func (s *Server) notificationRule(r *http.Request, p Principal) (Reply, error) {
	id, err := IDParam(r, "notification_rule_id")
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
	if err := p.Project(projectID, View); err != nil {
		return Reply{}, err
	}
	return Detail(json.RawMessage(data), etag), nil
}

// validateRuleSubject validates a budget threshold rule's subject, window and
// threshold, and normalizes its subject identifier.
func (s *Server) validateRuleSubject(r *http.Request, tx pgx.Tx, input *ruleInput) error {
	if input.SubjectKind == nil || *input.SubjectKind != "api_key" && *input.SubjectKind != "budget_group" {
		return Invalid("subject_kind", "Use api_key or budget_group.")
	}
	if input.WindowKind == nil || *input.WindowKind != "day" && *input.WindowKind != "month" && *input.WindowKind != "week" {
		return Invalid("window_kind", "Use day, week or month.")
	}
	if input.ThresholdPercent == nil || *input.ThresholdPercent < 1 || *input.ThresholdPercent > 100 {
		return Invalid("threshold_percent", "Use a threshold from 1 to 100.")
	}
	if input.SubjectID == nil {
		return Invalid("subject_id", "Use a valid subject identifier.")
	}
	return s.validateNotificationSubject(r, tx, input)
}

func (s *Server) validateNotificationSubject(r *http.Request, tx pgx.Tx, input *ruleInput) error {
	subjectID, err := ParseUUID(*input.SubjectID)
	if err != nil {
		return Invalid("subject_id", "Use a valid subject identifier.")
	}
	input.SubjectID = &subjectID
	var subjectProject *string
	switch *input.SubjectKind {
	case "api_key":
		err = tx.QueryRow(r.Context(),
			"SELECT project_id::text FROM olp.api_keys WHERE id=$1", subjectID).Scan(&subjectProject)
	case "budget_group":
		err = tx.QueryRow(r.Context(),
			"SELECT project_id::text FROM olp.budget_groups WHERE id=$1", subjectID).Scan(&subjectProject)
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

// validateRuleDestination validates that a rule's destination belongs to the
// rule's project, and normalizes its identifier.
func (s *Server) validateRuleDestination(r *http.Request, tx pgx.Tx, input *ruleInput) error {
	destinationID, err := ParseUUID(input.DestinationID)
	if err != nil {
		return Invalid("destination_id", "Use a valid destination identifier.")
	}
	input.DestinationID = destinationID
	var destinationProject *string
	if err = tx.QueryRow(r.Context(),
		"SELECT project_id::text FROM olp.notification_destinations WHERE id=$1", destinationID).Scan(&destinationProject); err != nil {
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

// validateRule validates a rule for its event, and normalizes its
// identifiers.
func projectScopedEvent(event string) bool {
	switch event {
	case "budget.exhausted", "route.latency", "model.retirement", "report.spend":
		return true
	}
	return false
}

func (s *Server) validateRule(r *http.Request, tx pgx.Tx, input *ruleInput) error {
	if err := ValidText("name", input.Name, 100); err != nil {
		return err
	}
	if !slices.Contains(notifications.Events, input.Event) {
		return Invalid("event", "Use a supported notification event.")
	}
	switch input.Event {
	case BudgetThresholdEvent:
		if err := s.validateRuleSubject(r, tx, input); err != nil {
			return err
		}
	case KeyExpiringEvent:
		if input.SubjectKind == nil || *input.SubjectKind != "api_key" || input.SubjectID == nil || input.WindowKind != nil || input.ThresholdPercent != nil {
			return Invalid("subject_id", "Key reminders require an API key and no budget window or threshold.")
		}
		if err := s.validateNotificationSubject(r, tx, input); err != nil {
			return err
		}
	case GrantLapsedEvent:
		if err := validateProviderEventRule(*input); err != nil {
			return err
		}
	default:
		if input.ProjectID != nil && !projectScopedEvent(input.Event) {
			return Invalid("project_id", "This notification event is installation-wide.")
		}
		for _, field := range []struct {
			name string
			set  bool
		}{
			{"subject_kind", input.SubjectKind != nil}, {"subject_id", input.SubjectID != nil},
			{"window_kind", input.WindowKind != nil}, {"threshold_percent", input.ThresholdPercent != nil},
		} {
			if field.set {
				return Invalid(field.name, "This notification event has no subject, window or threshold.")
			}
		}
	}
	configuration, err := notifications.ParseRuleConfiguration(input.Event, input.Configuration)
	if err != nil {
		return Invalid("configuration", "The rule configuration is invalid for its event.")
	}
	canonical, err := json.Marshal(configuration)
	if err != nil {
		return err
	}
	input.Configuration = canonical
	return s.validateRuleDestination(r, tx, input)
}

// validateProviderEventRule validates a rule subscribed to a provider event,
// which concerns the whole installation: it is installation-wide and watches
// no budget.
func validateProviderEventRule(input ruleInput) error {
	if input.ProjectID != nil {
		return Invalid("project_id", "Provider event rules are installation-wide.")
	}
	for _, field := range []struct {
		name string
		set  bool
	}{
		{"subject_kind", input.SubjectKind != nil}, {"subject_id", input.SubjectID != nil},
		{"window_kind", input.WindowKind != nil}, {"threshold_percent", input.ThresholdPercent != nil},
	} {
		if field.set {
			return Invalid(field.name, "Only budget.threshold rules watch a budget.")
		}
	}
	return nil
}

func (s *Server) createNotificationRule(r *http.Request, _ Principal) (Reply, error) {
	var input ruleInput
	if err := Decode(r, &input); err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Reauthorize(r, tx)
	if err != nil {
		return Reply{}, err
	}
	if err = p.Authorize(notificationOperation(input.ProjectID)); err != nil {
		return Reply{}, err
	}
	claim, replayed, err := s.Replay(r, tx, p, input)
	if err != nil {
		return Reply{}, err
	}
	if replayed != nil {
		// The stored reply carries the created rule, so the caller must
		// still reach its project to receive it.
		if err := s.RequireProject(r.Context(), tx, p, input.ProjectID); err != nil {
			return Reply{}, err
		}
		return Commit(r, tx, *replayed)
	}
	if input.ProjectID != nil {
		var parsed string
		if parsed, err = ParseUUID(*input.ProjectID); err != nil {
			return Reply{}, err
		}
		input.ProjectID = &parsed
	}
	if err = s.RequireProject(r.Context(), tx, p, input.ProjectID); err != nil {
		return Reply{}, err
	}
	if err = s.validateRule(r, tx, &input); err != nil {
		return Reply{}, err
	}
	switch input.Event {
	case BudgetThresholdEvent, KeyExpiringEvent, GrantLapsedEvent:
	default:
		if _, err = tx.Exec(r.Context(), lockGenericRuleCreationSQL); err != nil {
			return Reply{}, err
		}
		var count int
		if err = tx.QueryRow(r.Context(), genericRuleCountSQL).Scan(&count); err != nil {
			return Reply{}, err
		}
		if count >= 1000 {
			return Reply{}, Invalid("event", "The installation already has 1000 generic notification rules.")
		}
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	id, etag := NewID(), NewID()
	if _, err = tx.Exec(r.Context(), createChannelRuleSQL,
		id, strings.TrimSpace(input.Name), input.ProjectID, input.Event, input.SubjectKind, input.SubjectID,
		input.WindowKind, input.ThresholdPercent, input.DestinationID, enabled, etag, p.UserID(), input.Configuration); err != nil {
		return Reply{}, err
	}
	var data []byte
	if err = tx.QueryRow(r.Context(),
		"SELECT jsonb_build_object("+ruleFields+")"+ruleFrom+" WHERE r.id=$1", id).Scan(&data); err != nil {
		return Reply{}, err
	}
	result := Reply{Status: 201, ETag: etag, Location: "/api/v1/notifications/rules/" + id,
		Body: json.RawMessage(data)}
	if err = Audit(r.Context(), tx, r, p.Actor(), "notification_rule.create",
		"notification_rule", id, "success"); err != nil {
		return Reply{}, err
	}
	if err = s.CompleteReplay(r, tx, claim, result); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, result)
}

func (s *Server) updateNotificationRule(r *http.Request, _ Principal) (Reply, error) {
	var patch map[string]json.RawMessage
	if err := Decode(r, &patch); err != nil {
		return Reply{}, err
	}
	if len(patch) == 0 {
		return Reply{}, Invalid("rule", "Send at least one field to update.")
	}
	allowed := []string{"name", "subject_kind", "subject_id", "window_kind",
		"threshold_percent", "destination_id", "enabled", "configuration"}
	for field := range patch {
		if !slices.Contains(allowed, field) {
			return Reply{}, Invalid(field, "Unknown notification rule field.")
		}
	}
	id, err := IDParam(r, "notification_rule_id")
	if err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Reauthorize(r, tx)
	if err != nil {
		return Reply{}, err
	}
	var projectID *string
	if err = tx.QueryRow(r.Context(),
		"SELECT project_id::text FROM olp.notification_rules WHERE id=$1", id).Scan(&projectID); err != nil {
		return Reply{}, err
	}
	if err = p.Project(projectID, View); err != nil {
		return Reply{}, err
	}
	if err = p.Authorize(notificationOperation(projectID)); err != nil {
		return Reply{}, err
	}
	if err = p.Project(projectID, Change); err != nil {
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
				return Reply{}, Invalid("window_kind", "Use day, week or month.")
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
		case "configuration":
			if string(raw) == "null" {
				next.Configuration = json.RawMessage(`{}`)
			} else {
				var decoded map[string]json.RawMessage
				if err = json.Unmarshal(raw, &decoded); err != nil || decoded == nil {
					return Reply{}, Invalid("configuration", "Use a configuration object.")
				}
				next.Configuration = json.RawMessage(raw)
			}
		}
	}
	next.ProjectID = projectID
	if err = s.validateRule(r, tx, &next); err != nil {
		return Reply{}, err
	}
	enabled := next.Enabled != nil && *next.Enabled
	etag = NewID()
	if _, err = tx.Exec(r.Context(), updateChannelRuleSQL,
		id, strings.TrimSpace(next.Name), next.SubjectKind, next.SubjectID, next.WindowKind,
		next.ThresholdPercent, next.DestinationID, enabled, etag, next.Configuration); err != nil {
		return Reply{}, err
	}
	if err = tx.QueryRow(r.Context(),
		"SELECT jsonb_build_object("+ruleFields+")"+ruleFrom+" WHERE r.id=$1", id).Scan(&data); err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.Actor(), "notification_rule.update",
		"notification_rule", id, "success"); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, Detail(json.RawMessage(data), etag))
}

func (s *Server) notificationDeliveries(r *http.Request, p Principal) (Reply, error) {
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
