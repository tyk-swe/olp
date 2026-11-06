package notifications

import (
	"encoding/json"
	"net/http"

	"strings"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secrets"
)

const destinationFields = `'id',d.id,'name',d.name,'url',d.url,'type',d.type,'configuration',d.configuration,'secret_configured',d.secret_id IS NOT NULL,'project_id',d.project_id,'project_name',p.name,'enabled',d.enabled,'etag',d.etag,'created_by',d.created_by,'created_by_email',u.email,'created_at',d.created_at,'updated_at',d.updated_at`

const destinationFrom = ` FROM olp.notification_destinations d
	JOIN olp.users u ON u.id=d.created_by LEFT JOIN olp.projects p ON p.id=d.project_id`

type destinationInput struct {
	Name          string           `json:"name"`
	URL           string           `json:"url"`
	Type          string           `json:"type"`
	Configuration json.RawMessage  `json:"configuration"`
	ProjectID     *string          `json:"project_id"`
	Secret        *json.RawMessage `json:"secret"`
	Enabled       *bool            `json:"enabled"`
}

func (s *Server) notificationDestinations(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.access.Pool.Query(r.Context(),
		"SELECT jsonb_build_object("+destinationFields+")"+destinationFrom+
			" WHERE d.id<$1::uuid AND ($2 OR d.project_id=ANY($3::uuid[])) ORDER BY d.id DESC LIMIT $4",
		page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	items, err := access.JSONRows(rows)
	return access.ListReply(items, page), err
}

func (s *Server) notificationDestination(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "notification_destination_id")
	if err != nil {
		return access.Reply{}, err
	}
	var data []byte
	var etag string
	var projectID *string
	if err = s.access.Pool.QueryRow(r.Context(),
		"SELECT jsonb_build_object("+destinationFields+"),d.etag::text,d.project_id::text"+destinationFrom+" WHERE d.id=$1",
		id).Scan(&data, &etag, &projectID); err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(projectID, access.View); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(json.RawMessage(data), etag), nil
}

func (s *Server) validateDestination(input destinationInput, secret []byte) error {
	if err := access.ValidText("name", input.Name, 100); err != nil {
		return err
	}
	if s.egress == nil {
		return access.Fail(503, "egress_policy_unavailable", "Notification delivery is not configured on this installation.")
	}
	if len(input.Configuration) > 4096 {
		return access.Invalid("configuration", "Use a configuration object of at most 4096 bytes.")
	}
	d := Destination{Type: input.Type, URL: strings.TrimSpace(input.URL), Configuration: input.Configuration}
	if secret == nil {
		if err := ValidateDestinationShape(s.egress, d); err != nil {
			return access.Invalid("destination", "The destination or its configuration is invalid.")
		}
		if RequiresSecret(d.Type) && (input.Enabled == nil || *input.Enabled) {
			return access.Invalid("secret", "This destination type requires a credential secret object.")
		}
		return nil
	}
	if err := ValidateDestination(s.egress, d, secret); err != nil {
		return access.Invalid("destination", "The destination or its secret is invalid.")
	}
	return nil
}

// destinationSecret validates and decodes a notification destination secret.
func destinationSecret(typ string, raw json.RawMessage) ([]byte, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	if typ == "webhook" {
		var secret string
		if err := json.Unmarshal(raw, &secret); err != nil {
			return nil, access.Invalid("secret", "Use a signing secret string or null.")
		}
		if secret == "" || len(secret) > 1024 {
			return nil, access.Invalid("secret", "Use a signing secret of 1–1024 bytes.")
		}
		return []byte(secret), nil
	}
	if len(raw) > 16384 {
		return nil, access.Invalid("secret", "The secret object is too large.")
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded == nil {
		return nil, access.Invalid("secret", "Use the channel credential object or null.")
	}
	return raw, nil
}

func (s *Server) createNotificationDestination(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input destinationInput
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
		// The stored reply carries the created destination, so the caller
		// must still reach its project to receive it.
		if err := s.access.RequireProject(r.Context(), tx, p, input.ProjectID); err != nil {
			return access.Reply{}, err
		}
		return *replayed, tx.Commit(r.Context())
	}
	if input.Type == "" {
		input.Type = "webhook"
	}
	var secret []byte
	if input.Secret != nil {
		if secret, err = destinationSecret(input.Type, *input.Secret); err != nil {
			return access.Reply{}, err
		}
	}
	if err = s.validateDestination(input, secret); err != nil {
		return access.Reply{}, err
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
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	configuration := input.Configuration
	if len(configuration) == 0 || string(configuration) == "null" {
		configuration = json.RawMessage(`{}`)
	}
	id, etag := access.NewID(), access.NewID()
	if _, err = tx.Exec(r.Context(), createChannelDestinationSQL,
		id, strings.TrimSpace(input.Name), strings.TrimSpace(input.URL), input.ProjectID, etag,
		p.UserID(), enabled, input.Type, configuration); err != nil {
		return access.Reply{}, err
	}
	if input.Secret != nil {
		if err = s.storeNotificationSecret(r, tx, id, secret); err != nil {
			return access.Reply{}, err
		}
	}
	var data []byte
	if err = tx.QueryRow(r.Context(),
		"SELECT jsonb_build_object("+destinationFields+")"+destinationFrom+" WHERE d.id=$1", id).Scan(&data); err != nil {
		return access.Reply{}, err
	}
	result := access.Reply{Status: 201, ETag: etag, Location: "/api/v1/notifications/destinations/" + id,
		Body: json.RawMessage(data)}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "notification_destination.create",
		"notification_destination", id, "success"); err != nil {
		return access.Reply{}, err
	}
	if err = s.access.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return result, tx.Commit(r.Context())
}

func (s *Server) updateNotificationDestination(r *http.Request, _ access.Principal) (access.Reply, error) {
	var patch map[string]json.RawMessage
	if err := access.Decode(r, &patch); err != nil {
		return access.Reply{}, err
	}
	if len(patch) == 0 {
		return access.Reply{}, access.Invalid("destination", "Send at least one field to update.")
	}
	for field := range patch {
		if field != "name" && field != "url" && field != "type" && field != "configuration" && field != "enabled" && field != "secret" {
			return access.Reply{}, access.Invalid(field, "Unknown destination field.")
		}
	}
	id, err := access.IDParam(r, "notification_destination_id")
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
		"SELECT project_id::text FROM olp.notification_destinations WHERE id=$1", id).Scan(&projectID); err != nil {
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
		"SELECT jsonb_build_object("+destinationFields+"),d.etag::text"+destinationFrom+" WHERE d.id=$1 FOR UPDATE OF d",
		id).Scan(&data, &etag); err != nil {
		return access.Reply{}, err
	}
	if err = access.Match(r, etag); err != nil {
		return access.Reply{}, err
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
		return access.Reply{}, err
	}
	previousURL := current.URL
	if raw, ok := patch["name"]; ok {
		if err = json.Unmarshal(raw, &current.Name); err != nil {
			return access.Reply{}, access.Invalid("name", "Use a non-empty destination name.")
		}
		if err = access.ValidText("name", current.Name, 100); err != nil {
			return access.Reply{}, err
		}
	}
	if raw, ok := patch["url"]; ok {
		if err = json.Unmarshal(raw, &current.URL); err != nil {
			return access.Reply{}, access.Invalid("url", "Use a valid destination URL.")
		}
	}
	if raw, ok := patch["type"]; ok {
		var patched string
		if err = json.Unmarshal(raw, &patched); err != nil || patched != current.Type {
			return access.Reply{}, access.Invalid("type", "The destination type cannot be changed.")
		}
	}
	if raw, ok := patch["configuration"]; ok {
		if string(raw) == "null" {
			current.Configuration = json.RawMessage(`{}`)
		} else {
			current.Configuration = json.RawMessage(raw)
		}
		if len(current.Configuration) > 4096 {
			return access.Reply{}, access.Invalid("configuration", "Use a configuration object of at most 4096 bytes.")
		}
		var decoded map[string]json.RawMessage
		if err = json.Unmarshal(current.Configuration, &decoded); err != nil || decoded == nil {
			return access.Reply{}, access.Invalid("configuration", "Use a configuration object.")
		}
	}
	if raw, ok := patch["enabled"]; ok {
		if err = json.Unmarshal(raw, &current.Enabled); err != nil {
			return access.Reply{}, access.Invalid("enabled", "Use true or false.")
		}
	}
	if s.egress == nil {
		return access.Reply{}, access.Fail(503, "egress_policy_unavailable", "Notification delivery is not configured on this installation.")
	}
	d := Destination{Type: current.Type, URL: strings.TrimSpace(current.URL), Configuration: current.Configuration}
	raw, secretSupplied := patch["secret"]
	if !secretSupplied && current.SecretConfigured && d.URL != previousURL {
		return access.Reply{}, access.Invalid("secret", "Changing the destination URL requires a replacement secret or null.")
	}
	var effectiveSecret []byte
	if secretSupplied {
		if effectiveSecret, err = destinationSecret(current.Type, raw); err != nil {
			return access.Reply{}, err
		}
	} else if current.SecretConfigured {
		var secretID *string
		if err = tx.QueryRow(r.Context(), readDestinationSecretSQL, id).Scan(&secretID); err != nil {
			return access.Reply{}, err
		}
		if secretID != nil {
			if effectiveSecret, err = s.access.Keys.Read(r.Context(), tx, s.access.Installation, *secretID, secrets.NotificationSecret); err != nil {
				return access.Reply{}, err
			}
		}
	}
	secretCleared := secretSupplied && effectiveSecret == nil
	if len(effectiveSecret) != 0 || (!RequiresSecret(current.Type) && !secretCleared) {
		if err = ValidateDestination(s.egress, d, effectiveSecret); err != nil {
			return access.Reply{}, access.Invalid("destination", "The destination, configuration or secret is invalid.")
		}
	} else {
		if err = ValidateDestinationShape(s.egress, d); err != nil {
			return access.Reply{}, access.Invalid("destination", "The destination or its configuration is invalid.")
		}
		if current.Enabled && RequiresSecret(current.Type) {
			return access.Reply{}, access.Invalid("secret", "This destination type requires a credential secret object.")
		}
	}
	etag = access.NewID()
	if _, err = tx.Exec(r.Context(), updateChannelDestinationSQL,
		id, strings.TrimSpace(current.Name), strings.TrimSpace(current.URL), current.Enabled, etag,
		current.Type, current.Configuration); err != nil {
		return access.Reply{}, err
	}
	if secretSupplied {
		if err = s.storeNotificationSecret(r, tx, id, effectiveSecret); err != nil {
			return access.Reply{}, err
		}
	}
	if err = tx.QueryRow(r.Context(),
		"SELECT jsonb_build_object("+destinationFields+")"+destinationFrom+" WHERE d.id=$1", id).Scan(&data); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "notification_destination.update",
		"notification_destination", id, "success"); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(json.RawMessage(data), etag), tx.Commit(r.Context())
}

const createChannelDestinationSQL = `INSERT INTO olp.notification_destinations(id,name,url,project_id,etag,created_by,enabled,type,configuration)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`

const updateChannelDestinationSQL = `UPDATE olp.notification_destinations SET name=$2,url=$3,enabled=$4,etag=$5,type=$6,configuration=$7,updated_at=now() WHERE id=$1`

const readDestinationSecretSQL = `SELECT secret_id::text FROM olp.notification_destinations WHERE id=$1`
