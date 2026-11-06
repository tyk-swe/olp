package notifications

import (
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secrets"
	"net/http"
	"slices"
	"strings"
)

const ruleFields = `'id',r.id,'name',r.name,'project_id',r.project_id,'project_name',p.name,'event',r.event,'configuration',r.configuration,'subject_kind',r.subject_kind,'subject_id',r.subject_id,'subject_name',COALESCE(k.name,g.name),'window_kind',r.window_kind,'threshold_percent',r.threshold_percent,'destination_id',r.destination_id,'destination_name',d.name,'enabled',r.enabled,'etag',r.etag,'created_by',r.created_by,'created_by_email',u.email,'created_at',r.created_at,'updated_at',r.updated_at`

const ruleFrom = ` FROM olp.notification_rules r
	JOIN olp.users u ON u.id=r.created_by
	LEFT JOIN olp.projects p ON p.id=r.project_id
	LEFT JOIN olp.api_keys k ON r.subject_kind='api_key' AND k.id=r.subject_id
	LEFT JOIN olp.budget_groups g ON r.subject_kind='budget_group' AND g.id=r.subject_id
	JOIN olp.notification_destinations d ON d.id=r.destination_id`

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
func notificationOperation(projectID *string) access.Operation {
	if projectID == nil {
		return access.Settings
	}
	return access.Keys
}

func (s *Server) storeNotificationSecret(r *http.Request, tx pgx.Tx, id string, secret []byte) error {
	var secretID *string
	if secret != nil {
		stored := access.NewID()
		if err := s.access.Keys.Store(r.Context(), tx, s.access.Installation, stored, secrets.NotificationSecret, secret, nil); err != nil {
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

func (s *Server) notificationRules(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.access.Pool.Query(r.Context(),
		"SELECT jsonb_build_object("+ruleFields+")"+ruleFrom+
			" WHERE r.id<$1::uuid AND ($2 OR r.project_id=ANY($3::uuid[])) ORDER BY r.id DESC LIMIT $4",
		page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	items, err := access.JSONRows(rows)
	return access.ListReply(items, page), err
}

func (s *Server) notificationRule(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "notification_rule_id")
	if err != nil {
		return access.Reply{}, err
	}
	var data []byte
	var etag string
	var projectID *string
	if err = s.access.Pool.QueryRow(r.Context(),
		"SELECT jsonb_build_object("+ruleFields+"),r.etag::text,r.project_id::text"+ruleFrom+" WHERE r.id=$1",
		id).Scan(&data, &etag, &projectID); err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(projectID, access.View); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(json.RawMessage(data), etag), nil
}

// validateRuleSubject validates a budget threshold rule's subject, window and
// threshold, and normalizes its subject identifier.
func (s *Server) validateRuleSubject(r *http.Request, tx pgx.Tx, input *ruleInput) error {
	if input.SubjectKind == nil || *input.SubjectKind != "api_key" && *input.SubjectKind != "budget_group" {
		return access.Invalid("subject_kind", "Use api_key or budget_group.")
	}
	if input.WindowKind == nil || *input.WindowKind != "day" && *input.WindowKind != "month" && *input.WindowKind != "week" {
		return access.Invalid("window_kind", "Use day, week or month.")
	}
	if input.ThresholdPercent == nil || *input.ThresholdPercent < 1 || *input.ThresholdPercent > 100 {
		return access.Invalid("threshold_percent", "Use a threshold from 1 to 100.")
	}
	if input.SubjectID == nil {
		return access.Invalid("subject_id", "Use a valid subject identifier.")
	}
	return s.validateNotificationSubject(r, tx, input)
}

func (s *Server) validateNotificationSubject(r *http.Request, tx pgx.Tx, input *ruleInput) error {
	subjectID, err := access.ParseUUID(*input.SubjectID)
	if err != nil {
		return access.Invalid("subject_id", "Use a valid subject identifier.")
	}
	input.SubjectID = &subjectID
	var subjectProject *string
	switch *input.SubjectKind {
	case "api_key":
		err = tx.QueryRow(r.Context(),
			"SELECT project_id::text FROM olp.api_keys WHERE id=$1", subjectID).Scan(&subjectProject)
	case "budget_group":
		err = tx.QueryRow(r.Context(),
			"SELECT project_id::text FROM olp.budget_groups WHERE id=$1 AND retired_at IS NULL", subjectID).Scan(&subjectProject)
	}
	if err != nil {
		if err == pgx.ErrNoRows {
			return access.Invalid("subject_id", "The alert subject does not exist.")
		}
		return err
	}
	if (subjectProject == nil) != (input.ProjectID == nil) ||
		(subjectProject != nil && *subjectProject != *input.ProjectID) {
		return access.Invalid("subject_id", "The alert subject must belong to the rule's project.")
	}
	return nil
}

// validateRuleDestination validates that a rule's destination belongs to the
// rule's project, and normalizes its identifier.
func (s *Server) validateRuleDestination(r *http.Request, tx pgx.Tx, input *ruleInput) error {
	destinationID, err := access.ParseUUID(input.DestinationID)
	if err != nil {
		return access.Invalid("destination_id", "Use a valid destination identifier.")
	}
	input.DestinationID = destinationID
	var destinationProject *string
	if err = tx.QueryRow(r.Context(),
		"SELECT project_id::text FROM olp.notification_destinations WHERE id=$1", destinationID).Scan(&destinationProject); err != nil {
		if err == pgx.ErrNoRows {
			return access.Invalid("destination_id", "The notification destination does not exist.")
		}
		return err
	}
	if (destinationProject == nil) != (input.ProjectID == nil) ||
		(destinationProject != nil && *destinationProject != *input.ProjectID) {
		return access.Invalid("destination_id", "The notification destination must belong to the rule's project.")
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
	if err := access.ValidText("name", input.Name, 100); err != nil {
		return err
	}
	if !SupportsEvent(input.Event) {
		return access.Invalid("event", "Use a supported notification event.")
	}
	switch input.Event {
	case BudgetThresholdEvent:
		if err := s.validateRuleSubject(r, tx, input); err != nil {
			return err
		}
	case KeyExpiringEvent:
		if input.SubjectKind == nil || *input.SubjectKind != "api_key" || input.SubjectID == nil || input.WindowKind != nil || input.ThresholdPercent != nil {
			return access.Invalid("subject_id", "Key reminders require an API key and no budget window or threshold.")
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
			return access.Invalid("project_id", "This notification event is installation-wide.")
		}
		for _, field := range []struct {
			name string
			set  bool
		}{
			{"subject_kind", input.SubjectKind != nil}, {"subject_id", input.SubjectID != nil},
			{"window_kind", input.WindowKind != nil}, {"threshold_percent", input.ThresholdPercent != nil},
		} {
			if field.set {
				return access.Invalid(field.name, "This notification event has no subject, window or threshold.")
			}
		}
	}
	configuration, err := ParseRuleConfiguration(input.Event, input.Configuration)
	if err != nil {
		return access.Invalid("configuration", "The rule configuration is invalid for its event.")
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
		return access.Invalid("project_id", "Provider event rules are installation-wide.")
	}
	for _, field := range []struct {
		name string
		set  bool
	}{
		{"subject_kind", input.SubjectKind != nil}, {"subject_id", input.SubjectID != nil},
		{"window_kind", input.WindowKind != nil}, {"threshold_percent", input.ThresholdPercent != nil},
	} {
		if field.set {
			return access.Invalid(field.name, "Only budget.threshold rules watch a budget.")
		}
	}
	return nil
}

func (s *Server) createNotificationRule(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input ruleInput
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	tx, p, err := s.access.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	if err = p.Authorize(notificationOperation(input.ProjectID)); err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := s.access.Replay(r, tx, p, input)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		// The stored reply carries the created rule, so the caller must
		// still reach its project to receive it.
		if err := s.access.RequireProject(r.Context(), tx, p, input.ProjectID); err != nil {
			return access.Reply{}, err
		}
		return *replayed, tx.Commit(r.Context())
	}
	if input.ProjectID != nil {
		var parsed string
		if parsed, err = access.ParseUUID(*input.ProjectID); err != nil {
			return access.Reply{}, err
		}
		input.ProjectID = &parsed
	}
	if err = s.access.RequireProject(r.Context(), tx, p, input.ProjectID); err != nil {
		return access.Reply{}, err
	}
	if err = s.validateRule(r, tx, &input); err != nil {
		return access.Reply{}, err
	}
	switch input.Event {
	case BudgetThresholdEvent, KeyExpiringEvent, GrantLapsedEvent:
	default:
		if _, err = tx.Exec(r.Context(), lockGenericRuleCreationSQL); err != nil {
			return access.Reply{}, err
		}
		var count int
		if err = tx.QueryRow(r.Context(), genericRuleCountSQL).Scan(&count); err != nil {
			return access.Reply{}, err
		}
		if count >= 1000 {
			return access.Reply{}, access.Invalid("event", "The installation already has 1000 generic notification rules.")
		}
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	id, etag := access.NewID(), access.NewID()
	if _, err = tx.Exec(r.Context(), createChannelRuleSQL,
		id, strings.TrimSpace(input.Name), input.ProjectID, input.Event, input.SubjectKind, input.SubjectID,
		input.WindowKind, input.ThresholdPercent, input.DestinationID, enabled, etag, p.UserID(), input.Configuration); err != nil {
		return access.Reply{}, err
	}
	var data []byte
	if err = tx.QueryRow(r.Context(),
		"SELECT jsonb_build_object("+ruleFields+")"+ruleFrom+" WHERE r.id=$1", id).Scan(&data); err != nil {
		return access.Reply{}, err
	}
	result := access.Reply{Status: 201, ETag: etag, Location: "/api/v1/notifications/rules/" + id,
		Body: json.RawMessage(data)}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "notification_rule.create",
		"notification_rule", id, "success"); err != nil {
		return access.Reply{}, err
	}
	if err = s.access.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return result, tx.Commit(r.Context())
}

func (s *Server) updateNotificationRule(r *http.Request, _ access.Principal) (access.Reply, error) {
	var patch map[string]json.RawMessage
	if err := access.Decode(r, &patch); err != nil {
		return access.Reply{}, err
	}
	if len(patch) == 0 {
		return access.Reply{}, access.Invalid("rule", "Send at least one field to update.")
	}
	allowed := []string{"name", "subject_kind", "subject_id", "window_kind",
		"threshold_percent", "destination_id", "enabled", "configuration"}
	for field := range patch {
		if !slices.Contains(allowed, field) {
			return access.Reply{}, access.Invalid(field, "Unknown notification rule field.")
		}
	}
	id, err := access.IDParam(r, "notification_rule_id")
	if err != nil {
		return access.Reply{}, err
	}
	tx, p, err := s.access.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	var projectID *string
	if err = tx.QueryRow(r.Context(),
		"SELECT project_id::text FROM olp.notification_rules WHERE id=$1", id).Scan(&projectID); err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(projectID, access.View); err != nil {
		return access.Reply{}, err
	}
	if err = p.Authorize(notificationOperation(projectID)); err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(projectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	var data []byte
	var etag string
	if err = tx.QueryRow(r.Context(),
		"SELECT jsonb_build_object("+ruleFields+"),r.etag::text"+ruleFrom+" WHERE r.id=$1 FOR UPDATE OF r",
		id).Scan(&data, &etag); err != nil {
		return access.Reply{}, err
	}
	if err = access.Match(r, etag); err != nil {
		return access.Reply{}, err
	}
	var next ruleInput
	if err = json.Unmarshal(data, &next); err != nil {
		return access.Reply{}, err
	}
	for field, raw := range patch {
		switch field {
		case "name":
			if err = json.Unmarshal(raw, &next.Name); err != nil {
				return access.Reply{}, access.Invalid("name", "Use a non-empty rule name.")
			}
		case "subject_kind":
			if err = json.Unmarshal(raw, &next.SubjectKind); err != nil {
				return access.Reply{}, access.Invalid("subject_kind", "Use api_key or budget_group.")
			}
		case "subject_id":
			if err = json.Unmarshal(raw, &next.SubjectID); err != nil {
				return access.Reply{}, access.Invalid("subject_id", "Use a valid subject identifier.")
			}
		case "window_kind":
			if err = json.Unmarshal(raw, &next.WindowKind); err != nil {
				return access.Reply{}, access.Invalid("window_kind", "Use day, week or month.")
			}
		case "threshold_percent":
			if err = json.Unmarshal(raw, &next.ThresholdPercent); err != nil {
				return access.Reply{}, access.Invalid("threshold_percent", "Use a threshold from 1 to 100.")
			}
		case "destination_id":
			if err = json.Unmarshal(raw, &next.DestinationID); err != nil {
				return access.Reply{}, access.Invalid("destination_id", "Use a valid destination identifier.")
			}
		case "enabled":
			if err = json.Unmarshal(raw, &next.Enabled); err != nil {
				return access.Reply{}, access.Invalid("enabled", "Use true or false.")
			}
		case "configuration":
			if string(raw) == "null" {
				next.Configuration = json.RawMessage(`{}`)
			} else {
				var decoded map[string]json.RawMessage
				if err = json.Unmarshal(raw, &decoded); err != nil || decoded == nil {
					return access.Reply{}, access.Invalid("configuration", "Use a configuration object.")
				}
				next.Configuration = json.RawMessage(raw)
			}
		}
	}
	next.ProjectID = projectID
	if err = s.validateRule(r, tx, &next); err != nil {
		return access.Reply{}, err
	}
	enabled := next.Enabled != nil && *next.Enabled
	etag = access.NewID()
	if _, err = tx.Exec(r.Context(), updateChannelRuleSQL,
		id, strings.TrimSpace(next.Name), next.SubjectKind, next.SubjectID, next.WindowKind,
		next.ThresholdPercent, next.DestinationID, enabled, etag, next.Configuration); err != nil {
		return access.Reply{}, err
	}
	if err = tx.QueryRow(r.Context(),
		"SELECT jsonb_build_object("+ruleFields+")"+ruleFrom+" WHERE r.id=$1", id).Scan(&data); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "notification_rule.update",
		"notification_rule", id, "success"); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(json.RawMessage(data), etag), tx.Commit(r.Context())
}

const createChannelRuleSQL = `INSERT INTO olp.notification_rules(id,name,project_id,event,subject_kind,subject_id,window_kind,threshold_percent,destination_id,enabled,etag,created_by,configuration)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`

const updateChannelRuleSQL = `UPDATE olp.notification_rules SET name=$2,subject_kind=$3,subject_id=$4,window_kind=$5,threshold_percent=$6,destination_id=$7,enabled=$8,etag=$9,configuration=$10,updated_at=now() WHERE id=$1`

const genericRuleCountSQL = `SELECT count(*) FROM olp.notification_rules WHERE event NOT IN ('budget.threshold','provider.grant.lapsed','key.expiring')`

const lockGenericRuleCreationSQL = `SELECT pg_advisory_xact_lock(hashtextextended('olp.notification_rule_creation',0))`
