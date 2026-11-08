package notifications

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
)

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
	event := contract.NotificationEvent(input.Event)
	spec, ok := events[event]
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
	configuration, err := spec.configuration.parse(input.Configuration)
	if err != nil {
		return ruleTarget{}, access.Invalid("configuration", "The rule configuration is invalid for its event.")
	}
	canonical, err := json.Marshal(configuration)
	if err != nil {
		return ruleTarget{}, err
	}
	destinationID, err := access.ParseUUID(input.DestinationID)
	if err != nil {
		return ruleTarget{}, access.Invalid("destination_id", "Use a valid destination identifier.")
	}
	return ruleTarget{
		name: strings.TrimSpace(input.Name), projectID: projectID, event: event, subject: subject,
		destinationID: destinationID, enabled: input.Enabled == nil || *input.Enabled, configuration: canonical,
	}, nil
}

func parseSubject(shape subjectShape, input ruleInput) (ruleSubject, error) {
	switch shape {
	case budgetSubject:
		return parseBudgetWatch(input)
	case keySubject:
		if input.SubjectKind == nil || *input.SubjectKind != string(contract.BudgetSubjectKindApiKey) ||
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
	if input.SubjectKind == nil || !contract.BudgetSubjectKind(*input.SubjectKind).Valid() {
		return nil, access.Invalid("subject_kind", "Use api_key or budget_group.")
	}
	if input.WindowKind == nil || !contract.BudgetWindowKind(*input.WindowKind).Valid() {
		return nil, access.Invalid("window_kind", "Use day, week or month.")
	}
	if input.ThresholdPercent == nil || *input.ThresholdPercent < 1 || *input.ThresholdPercent > 100 {
		return nil, access.Invalid("threshold_percent", "Use a threshold from 1 to 100.")
	}
	if input.SubjectID == nil {
		return nil, access.Invalid("subject_id", "Use a valid subject identifier.")
	}
	subject, err := parseWatched(contract.BudgetSubjectKind(*input.SubjectKind), *input.SubjectID)
	if err != nil {
		return nil, err
	}
	return budgetWatch{watchedSubject: subject, window: contract.BudgetWindowKind(*input.WindowKind), percent: *input.ThresholdPercent}, nil
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
		query := apiKeyProjectSQL
		if subject.kind == contract.BudgetSubjectKindBudgetGroup {
			query = budgetGroupProjectSQL
		}
		project, found, err := referenceProject(ctx, tx, query, subject.id)
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
	if events[event].evaluate == nil {
		return nil
	}
	if _, err := tx.Exec(ctx, lockGenericRuleCreationSQL); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRow(ctx, evaluatedRuleCountSQL, evaluatedEvents).Scan(&count); err != nil {
		return err
	}
	if count >= evaluatedRuleLimit {
		return access.Invalid("event", "The installation already has 1000 generic notification rules.")
	}
	return nil
}

func (s *Server) createNotificationRule(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input ruleInput
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	return s.create(r, rules, creation{projectID: input.ProjectID, fingerprint: input,
		insert: func(tx pgx.Tx, p access.Principal, projectID *string, id, etag string) error {
			target, err := parseRule(input, projectID)
			if err != nil {
				return err
			}
			if err = checkReferences(r.Context(), tx, target); err != nil {
				return err
			}
			if err = enforceRuleCap(r.Context(), tx, target.event); err != nil {
				return err
			}
			c := target.subject.columns()
			_, err = tx.Exec(r.Context(), createChannelRuleSQL, id, target.name, target.projectID, target.event,
				c.kind, c.id, c.window, c.percent, target.destinationID, target.enabled, etag, p.UserID(), target.configuration)
			return err
		}})
}

func (s *Server) updateNotificationRule(r *http.Request, _ access.Principal) (access.Reply, error) {
	var patch map[string]json.RawMessage
	if err := access.Decode(r, &patch); err != nil {
		return access.Reply{}, err
	}
	if len(patch) == 0 {
		return access.Reply{}, access.Invalid("rule", "Send at least one field to update.")
	}
	for field := range patch {
		if _, ok := rulePatchFields[field]; !ok {
			return access.Reply{}, access.Invalid(field, "Unknown notification rule field.")
		}
	}
	return s.update(r, rules, func(tx pgx.Tx, row locked, etag string) error {
		input, err := patchRule(row.current, patch)
		if err != nil {
			return err
		}
		target, err := parseRule(input, row.projectID)
		if err != nil {
			return err
		}
		if err = checkReferences(r.Context(), tx, target); err != nil {
			return err
		}
		c := target.subject.columns()
		_, err = tx.Exec(r.Context(), updateChannelRuleSQL, row.id, target.name,
			c.kind, c.id, c.window, c.percent, target.destinationID, target.enabled, etag, target.configuration)
		return err
	})
}

func patchRule(current json.RawMessage, patch map[string]json.RawMessage) (ruleInput, error) {
	var next ruleInput
	if err := json.Unmarshal(current, &next); err != nil {
		return ruleInput{}, err
	}
	for field, raw := range patch {
		object, err := json.Marshal(map[string]json.RawMessage{field: raw})
		if err != nil {
			return ruleInput{}, err
		}
		if err = json.Unmarshal(object, &next); err != nil {
			return ruleInput{}, access.Invalid(field, rulePatchFields[field])
		}
	}
	return next, nil
}
