package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
)

type ruleInput struct {
	Name             string                      `json:"name"`
	ProjectID        *string                     `json:"project_id"`
	Event            contract.NotificationEvent  `json:"event"`
	SubjectKind      *contract.BudgetSubjectKind `json:"subject_kind"`
	SubjectID        *string                     `json:"subject_id"`
	WindowKind       *contract.BudgetWindowKind  `json:"window_kind"`
	ThresholdPercent *int                        `json:"threshold_percent"`
	DestinationID    string                      `json:"destination_id"`
	Enabled          *bool                       `json:"enabled"`
	Configuration    json.RawMessage             `json:"configuration"`
}

var rulePatchFields = map[string]string{
	"name":              "Use a non-empty rule name.",
	"subject_kind":      "Use api_key or budget_group.",
	"subject_id":        "Use a valid subject identifier.",
	"window_kind":       "Use day, week or month.",
	"threshold_percent": "Use a threshold from 1 to 100.",
	"destination_id":    "Use a valid destination identifier.",
	"enabled":           "Use true or false.",
	"configuration":     "Use a configuration object.",
}

type subjectShape int

const (
	noSubject subjectShape = iota
	budgetSubject
	keySubject
)

type watchedSubject struct {
	kind contract.BudgetSubjectKind
	id   string
}

type ruleSubject interface {
	columns() subjectColumns
}

type subjectColumns struct {
	kind, id, window *string
	percent          *int
}

type unwatched struct{}

type budgetWatch struct {
	watchedSubject
	window  contract.BudgetWindowKind
	percent int
}

type keyWatch struct{ watchedSubject }

func (unwatched) columns() subjectColumns { return subjectColumns{} }

func (b budgetWatch) columns() subjectColumns {
	kind, window := string(b.kind), string(b.window)
	return subjectColumns{kind: &kind, id: &b.id, window: &window, percent: &b.percent}
}

func (k keyWatch) columns() subjectColumns {
	kind := string(k.kind)
	return subjectColumns{kind: &kind, id: &k.id}
}

type ruleTarget struct {
	name          string
	projectID     *string
	event         contract.NotificationEvent
	subject       ruleSubject
	destinationID string
	enabled       bool
	configuration json.RawMessage
}

func parseRule(input ruleInput, projectID *string) (ruleTarget, error) {
	if err := access.ValidText("name", input.Name, nameLimit); err != nil {
		return ruleTarget{}, err
	}
	spec, ok := events[input.Event]
	if !ok {
		return ruleTarget{}, access.Invalid("event", "Use a supported notification event.")
	}
	subject, err := parseSubject(spec.subject, input)
	if err != nil {
		return ruleTarget{}, err
	}
	if spec.subject == noSubject && projectID != nil && !spec.projectScoped {
		return ruleTarget{}, access.Invalid("project_id", "This notification event is installation-wide.")
	}
	configuration, err := spec.configure(input.Configuration)
	if err != nil {
		return ruleTarget{}, access.Invalid("configuration", "The rule configuration is invalid for its event.")
	}
	destinationID, err := access.ParseUUID(input.DestinationID)
	if err != nil {
		return ruleTarget{}, access.Invalid("destination_id", "Use a valid destination identifier.")
	}
	return ruleTarget{
		name: strings.TrimSpace(input.Name), projectID: projectID, event: input.Event, subject: subject,
		destinationID: destinationID, enabled: input.Enabled == nil || *input.Enabled, configuration: configuration.canonical,
	}, nil
}

func parseSubject(shape subjectShape, input ruleInput) (ruleSubject, error) {
	switch shape {
	case budgetSubject:
		return parseBudgetWatch(input)
	case keySubject:
		if input.SubjectKind == nil || *input.SubjectKind != contract.BudgetSubjectKindApiKey ||
			input.SubjectID == nil || input.WindowKind != nil || input.ThresholdPercent != nil {
			return nil, access.Invalid("subject_id", "Key reminders require an API key and no budget window or threshold.")
		}
		subject, err := parseWatched(contract.BudgetSubjectKindApiKey, *input.SubjectID)
		if err != nil {
			return nil, err
		}
		return keyWatch{subject}, nil
	}
	for _, field := range []struct {
		name string
		set  bool
	}{
		{"subject_kind", input.SubjectKind != nil}, {"subject_id", input.SubjectID != nil},
		{"window_kind", input.WindowKind != nil}, {"threshold_percent", input.ThresholdPercent != nil},
	} {
		if field.set {
			return nil, access.Invalid(field.name, "This notification event has no subject, window or threshold.")
		}
	}
	return unwatched{}, nil
}

func parseBudgetWatch(input ruleInput) (ruleSubject, error) {
	if input.SubjectKind == nil || !input.SubjectKind.Valid() {
		return nil, access.Invalid("subject_kind", "Use api_key or budget_group.")
	}
	if input.WindowKind == nil || !input.WindowKind.Valid() {
		return nil, access.Invalid("window_kind", "Use day, week or month.")
	}
	if input.ThresholdPercent == nil || *input.ThresholdPercent < 1 || *input.ThresholdPercent > 100 {
		return nil, access.Invalid("threshold_percent", "Use a threshold from 1 to 100.")
	}
	if input.SubjectID == nil {
		return nil, access.Invalid("subject_id", "Use a valid subject identifier.")
	}
	subject, err := parseWatched(*input.SubjectKind, *input.SubjectID)
	if err != nil {
		return nil, err
	}
	return budgetWatch{watchedSubject: subject, window: *input.WindowKind, percent: *input.ThresholdPercent}, nil
}

func parseWatched(kind contract.BudgetSubjectKind, raw string) (watchedSubject, error) {
	id, err := access.ParseUUID(raw)
	if err != nil {
		return watchedSubject{}, access.Invalid("subject_id", "Use a valid subject identifier.")
	}
	return watchedSubject{kind: kind, id: id}, nil
}

func watched(subject ruleSubject) (watchedSubject, bool) {
	switch s := subject.(type) {
	case budgetWatch:
		return s.watchedSubject, true
	case keyWatch:
		return s.watchedSubject, true
	}
	return watchedSubject{}, false
}

func checkReferences(ctx context.Context, tx pgx.Tx, t ruleTarget) error {
	if subject, ok := watched(t.subject); ok {
		project, found, err := referenceProject(ctx, tx, subjectProjectSQL[subject.kind], subject.id)
		if err != nil {
			return err
		}
		if !found {
			return access.Invalid("subject_id", "The alert subject does not exist.")
		}
		if !sameProject(project, t.projectID) {
			return access.Invalid("subject_id", "The alert subject must belong to the rule's project.")
		}
	}
	project, found, err := referenceProject(ctx, tx, destinationProjectSQL, t.destinationID)
	if err != nil {
		return err
	}
	if !found {
		return access.Invalid("destination_id", "The notification destination does not exist.")
	}
	if !sameProject(project, t.projectID) {
		return access.Invalid("destination_id", "The notification destination must belong to the rule's project.")
	}
	return nil
}

func enforceRuleCap(ctx context.Context, tx pgx.Tx, event contract.NotificationEvent) error {
	if !slices.Contains(evaluatedEvents, string(event)) {
		return nil
	}
	if _, err := tx.Exec(ctx, ruleCreationLockSQL, ruleCreationLockName); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRow(ctx, evaluatedRuleCountSQL, evaluatedEvents).Scan(&count); err != nil {
		return err
	}
	if count >= evaluatedRuleLimit {
		return access.Invalid("event", fmt.Sprintf("The installation already has %d generic notification rules.", evaluatedRuleLimit))
	}
	return nil
}

func (s *Server) createNotificationRule(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input ruleInput
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	return create(s, r, rules, creation[ruleTarget]{projectID: input.ProjectID, fingerprint: input,
		parse: func(projectID *string) (ruleTarget, error) { return parseRule(input, projectID) },
		insert: func(tx pgx.Tx, p access.Principal, _ *string, target ruleTarget, id, etag string) error {
			if err := checkReferences(r.Context(), tx, target); err != nil {
				return err
			}
			if err := enforceRuleCap(r.Context(), tx, target.event); err != nil {
				return err
			}
			c := target.subject.columns()
			_, err := tx.Exec(r.Context(), createChannelRuleSQL, id, target.name, target.projectID, target.event,
				c.kind, c.id, c.window, c.percent, target.destinationID, target.enabled, etag, p.UserID(), target.configuration)
			return err
		}})
}

func (s *Server) updateNotificationRule(r *http.Request, _ access.Principal) (access.Reply, error) {
	patch, err := decodePatch(r, "rule", "Unknown notification rule field.", rulePatchFields)
	if err != nil {
		return access.Reply{}, err
	}
	return s.update(r, rules, func(tx pgx.Tx, row document, etag string) error {
		input, err := patchRule(row.body, patch)
		if err != nil {
			return err
		}
		target, err := parseRule(input, row.projectID)
		if err != nil {
			return err
		}
		if err := checkReferences(r.Context(), tx, target); err != nil {
			return err
		}
		c := target.subject.columns()
		if _, err := tx.Exec(r.Context(), updateChannelRuleSQL, row.id, target.name,
			c.kind, c.id, c.window, c.percent, target.destinationID, target.enabled, etag, target.configuration); err != nil {
			return err
		}
		return nil
	})
}

func patchRule(current json.RawMessage, patch map[string]json.RawMessage) (ruleInput, error) {
	var next ruleInput
	if err := json.Unmarshal(current, &next); err != nil {
		return ruleInput{}, err
	}
	if string(patch["enabled"]) == "null" {
		return ruleInput{}, access.Invalid("enabled", rulePatchFields["enabled"])
	}
	overlay, err := json.Marshal(patch)
	if err != nil {
		return ruleInput{}, err
	}
	if err := json.Unmarshal(overlay, &next); err != nil {
		var mismatch *json.UnmarshalTypeError
		if errors.As(err, &mismatch) {
			return ruleInput{}, access.Invalid(mismatch.Field, rulePatchFields[mismatch.Field])
		}
		return ruleInput{}, err
	}
	return next, nil
}

var rules = resource{
	parameter: "notification_rule_id", kind: "notification_rule", path: "/api/v1/notifications/rules/",
	documentsSQL: ruleDocumentsSQL, lockSQL: lockRuleSQL, remove: removeRule,
}

func removeRule(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, "DELETE FROM olp.notification_rules WHERE id=$1", id)
	return err
}

func referenceProject(ctx context.Context, tx pgx.Tx, query, id string) (*string, bool, error) {
	var project *string
	err := tx.QueryRow(ctx, query, id).Scan(&project)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	return project, err == nil, err
}

func sameProject(a, b *string) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

const ruleDocumentsSQL = `SELECT r.id,r.project_id,r.etag,jsonb_build_object(
	'id',r.id,'name',r.name,'project_id',r.project_id,'project_name',p.name,'event',r.event,'configuration',r.configuration,
	'subject_kind',r.subject_kind,'subject_id',r.subject_id,'subject_name',COALESCE(k.name,g.name),
	'window_kind',r.window_kind,'threshold_percent',r.threshold_percent,'destination_id',r.destination_id,'destination_name',d.name,
	'enabled',r.enabled,'etag',r.etag,'created_by',r.created_by,'created_by_email',u.email,'created_at',r.created_at,'updated_at',r.updated_at) AS document
 FROM olp.notification_rules r
 JOIN olp.users u ON u.id=r.created_by
 LEFT JOIN olp.projects p ON p.id=r.project_id
 LEFT JOIN olp.api_keys k ON r.subject_kind='api_key' AND k.id=r.subject_id
 LEFT JOIN olp.budget_groups g ON r.subject_kind='budget_group' AND g.id=r.subject_id
 JOIN olp.notification_destinations d ON d.id=r.destination_id`

const lockRuleSQL = `SELECT etag::text,project_id::text FROM olp.notification_rules WHERE id=$1 FOR UPDATE`

const createChannelRuleSQL = `INSERT INTO olp.notification_rules(id,name,project_id,event,subject_kind,subject_id,window_kind,threshold_percent,destination_id,enabled,etag,created_by,configuration)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`

const updateChannelRuleSQL = `UPDATE olp.notification_rules SET name=$2,subject_kind=$3,subject_id=$4,window_kind=$5,threshold_percent=$6,destination_id=$7,enabled=$8,etag=$9,configuration=$10,updated_at=now() WHERE id=$1`

const evaluatedRuleCountSQL = `SELECT count(*) FROM olp.notification_rules WHERE event=ANY($1)`

const destinationProjectSQL = `SELECT project_id::text FROM olp.notification_destinations WHERE id=$1`

var subjectProjectSQL = map[contract.BudgetSubjectKind]string{
	contract.BudgetSubjectKindApiKey:      `SELECT project_id::text FROM olp.api_keys WHERE id=$1`,
	contract.BudgetSubjectKindBudgetGroup: `SELECT project_id::text FROM olp.budget_groups WHERE id=$1 AND retired_at IS NULL`,
}
