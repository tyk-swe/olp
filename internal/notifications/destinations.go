package notifications

import (
	"cmp"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secrets"
)

type destinationInput struct {
	Name          string           `json:"name"`
	URL           string           `json:"url"`
	Type          string           `json:"type"`
	Configuration json.RawMessage  `json:"configuration"`
	ProjectID     *string          `json:"project_id"`
	Secret        *json.RawMessage `json:"secret"`
	Enabled       *bool            `json:"enabled"`
}

type storedDestination struct {
	Name             string          `json:"name"`
	URL              string          `json:"url"`
	Type             string          `json:"type"`
	Configuration    json.RawMessage `json:"configuration"`
	Enabled          bool            `json:"enabled"`
	SecretConfigured bool            `json:"secret_configured"`
}

var destinationPatchFields = map[string]bool{"name": true, "url": true, "type": true, "configuration": true, "enabled": true, "secret": true}

func destinationSecret(typ string, raw json.RawMessage) ([]byte, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	if typ == webhookType {
		var secret string
		if err := json.Unmarshal(raw, &secret); err != nil {
			return nil, access.Invalid("secret", "Use a signing secret string or null.")
		}
		if secret == "" || len(secret) > signingSecretLimit {
			return nil, access.Invalid("secret", "Use a signing secret of 1–1024 bytes.")
		}
		return []byte(secret), nil
	}
	if len(raw) > secretObjectLimit {
		return nil, access.Invalid("secret", "The secret object is too large.")
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded == nil {
		return nil, access.Invalid("secret", "Use the channel credential object or null.")
	}
	return raw, nil
}

func configurationObject(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage(`{}`), nil
	}
	if len(raw) > configurationLimit {
		return nil, access.Invalid("configuration", "Use a configuration object of at most 4096 bytes.")
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded == nil {
		return nil, access.Invalid("configuration", "Use a configuration object.")
	}
	return raw, nil
}

func (s *Server) checkDestination(d destination, secret []byte, bind, enabled bool) error {
	if bind {
		if _, err := open(&s.egress, d, secret); err != nil {
			return access.Invalid("destination", "The destination, configuration or secret is invalid.")
		}
		return nil
	}
	if _, err := parse(&s.egress, d); err != nil {
		return access.Invalid("destination", "The destination or its configuration is invalid.")
	}
	if enabled && requiresSecret(d.Type) {
		return access.Invalid("secret", "This destination type requires a credential secret object.")
	}
	return nil
}

func (s *Server) createNotificationDestination(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input destinationInput
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	return s.create(r, destinations, creation{projectID: input.ProjectID, fingerprint: input,
		insert: func(tx pgx.Tx, p access.Principal, projectID *string, id, etag string) error {
			if err := access.ValidText("name", input.Name, nameLimit); err != nil {
				return err
			}
			configuration, err := configurationObject(input.Configuration)
			if err != nil {
				return err
			}
			typ := cmp.Or(input.Type, webhookType)
			secret, err := suppliedSecret(typ, input.Secret)
			if err != nil {
				return err
			}
			enabled := input.Enabled == nil || *input.Enabled
			target := destination{Type: typ, URL: strings.TrimSpace(input.URL), Configuration: configuration}
			if err = s.checkDestination(target, secret, secret != nil, enabled); err != nil {
				return err
			}
			if _, err = tx.Exec(r.Context(), createChannelDestinationSQL,
				id, strings.TrimSpace(input.Name), target.URL, projectID, etag, p.UserID(), enabled, typ, configuration); err != nil {
				return err
			}
			if input.Secret == nil {
				return nil
			}
			return s.storeNotificationSecret(r, tx, id, secret)
		}})
}

func suppliedSecret(typ string, raw *json.RawMessage) ([]byte, error) {
	if raw == nil {
		return nil, nil
	}
	return destinationSecret(typ, *raw)
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
		if !destinationPatchFields[field] {
			return access.Reply{}, access.Invalid(field, "Unknown destination field.")
		}
	}
	return s.update(r, destinations, func(tx pgx.Tx, row locked, etag string) error {
		var current storedDestination
		if err := json.Unmarshal(row.current, &current); err != nil {
			return err
		}
		next, err := patchDestination(current, patch)
		if err != nil {
			return err
		}
		target := destination{Type: next.Type, URL: strings.TrimSpace(next.URL), Configuration: next.Configuration}
		rawSecret, secretSupplied := patch["secret"]
		if !secretSupplied && current.SecretConfigured && target.URL != current.URL {
			return access.Invalid("secret", "Changing the destination URL requires a replacement secret or null.")
		}
		secret, err := s.effectiveSecret(r, tx, row.id, current, rawSecret, secretSupplied)
		if err != nil {
			return err
		}
		cleared := secretSupplied && secret == nil
		bind := len(secret) != 0 || !requiresSecret(next.Type) && !cleared
		if err = s.checkDestination(target, secret, bind, next.Enabled); err != nil {
			return err
		}
		if _, err = tx.Exec(r.Context(), updateChannelDestinationSQL,
			row.id, strings.TrimSpace(next.Name), target.URL, next.Enabled, etag, next.Type, next.Configuration); err != nil {
			return err
		}
		if !secretSupplied {
			return nil
		}
		return s.storeNotificationSecret(r, tx, row.id, secret)
	})
}

func patchDestination(current storedDestination, patch map[string]json.RawMessage) (storedDestination, error) {
	next := current
	if raw, ok := patch["name"]; ok {
		if err := json.Unmarshal(raw, &next.Name); err != nil {
			return next, access.Invalid("name", "Use a non-empty destination name.")
		}
		if err := access.ValidText("name", next.Name, nameLimit); err != nil {
			return next, err
		}
	}
	if raw, ok := patch["url"]; ok {
		if err := json.Unmarshal(raw, &next.URL); err != nil {
			return next, access.Invalid("url", "Use a valid destination URL.")
		}
	}
	if raw, ok := patch["type"]; ok {
		var patched string
		if err := json.Unmarshal(raw, &patched); err != nil || patched != current.Type {
			return next, access.Invalid("type", "The destination type cannot be changed.")
		}
	}
	if raw, ok := patch["configuration"]; ok {
		configuration, err := configurationObject(raw)
		if err != nil {
			return next, err
		}
		next.Configuration = configuration
	}
	if raw, ok := patch["enabled"]; ok {
		if err := json.Unmarshal(raw, &next.Enabled); err != nil {
			return next, access.Invalid("enabled", "Use true or false.")
		}
	}
	return next, nil
}

func (s *Server) effectiveSecret(r *http.Request, tx pgx.Tx, id string, current storedDestination, raw json.RawMessage, supplied bool) ([]byte, error) {
	if supplied {
		return destinationSecret(current.Type, raw)
	}
	if !current.SecretConfigured {
		return nil, nil
	}
	var secretID *string
	if err := tx.QueryRow(r.Context(), readDestinationSecretSQL, id).Scan(&secretID); err != nil || secretID == nil {
		return nil, err
	}
	return s.access.Keys.Read(r.Context(), tx, s.access.Installation, *secretID, secrets.NotificationSecret)
}
