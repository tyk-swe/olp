package notifications

import (
	"cmp"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/secrets"
)

type destinationInput struct {
	Name          string                               `json:"name"`
	URL           string                               `json:"url"`
	Type          contract.NotificationDestinationType `json:"type"`
	Configuration json.RawMessage                      `json:"configuration"`
	ProjectID     *string                              `json:"project_id"`
	Secret        json.RawMessage                      `json:"secret"`
	Enabled       *bool                                `json:"enabled"`
}

type storedDestination struct {
	Name             string                               `json:"name"`
	URL              string                               `json:"url"`
	Type             contract.NotificationDestinationType `json:"type"`
	Configuration    map[string]json.RawMessage           `json:"configuration"`
	Enabled          bool                                 `json:"enabled"`
	SecretConfigured bool                                 `json:"secret_configured"`
}

type secretChange interface{ secretChange() }

type keptSecret struct{}

type newSecret struct{ value []byte }

func (keptSecret) secretChange() {}
func (newSecret) secretChange()  {}

type destinationTarget struct {
	destination
	name    string
	enabled bool
	secret  secretChange
}

var destinationPatchFields = map[string]bool{"name": true, "url": true, "type": true, "configuration": true, "enabled": true, "secret": true}

func destinationSecret(typ contract.NotificationDestinationType, raw json.RawMessage) (secretChange, error) {
	if len(raw) == 0 {
		return keptSecret{}, nil
	}
	if string(raw) == "null" {
		return newSecret{}, nil
	}
	if typ == contract.NotificationDestinationTypeWebhook {
		var secret string
		if err := json.Unmarshal(raw, &secret); err != nil {
			return nil, access.Invalid("secret", "Use a signing secret string or null.")
		}
		if secret == "" || len(secret) > signingSecretLimit {
			return nil, access.Invalid("secret", "Use a signing secret of 1–1024 bytes.")
		}
		return newSecret{[]byte(secret)}, nil
	}
	if len(raw) > secretObjectLimit {
		return nil, access.Invalid("secret", "The secret object is too large.")
	}
	return newSecret{raw}, nil
}

func configurationObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if len(raw) > configurationLimit {
		return nil, access.Invalid("configuration", "Use a configuration object of at most 4096 bytes.")
	}
	fields, err := decodeObject(raw)
	if err != nil {
		return nil, access.Invalid("configuration", "Use a configuration object.")
	}
	return fields, nil
}

func parseDestination(input destinationInput) (destinationTarget, error) {
	if err := access.ValidText("name", input.Name, nameLimit); err != nil {
		return destinationTarget{}, err
	}
	configuration, err := configurationObject(input.Configuration)
	if err != nil {
		return destinationTarget{}, err
	}
	typ := cmp.Or(input.Type, contract.NotificationDestinationTypeWebhook)
	secret, err := destinationSecret(typ, input.Secret)
	if err != nil {
		return destinationTarget{}, err
	}
	return destinationTarget{
		destination: destination{Type: typ, URL: strings.TrimSpace(input.URL), Configuration: configuration},
		name:        strings.TrimSpace(input.Name),
		enabled:     input.Enabled == nil || *input.Enabled,
		secret:      secret,
	}, nil
}

func (s *Server) checkEndpoint(d destination, enabled bool) error {
	if _, err := parse(&s.egress, d); err != nil {
		return access.Invalid("destination", "The destination or its configuration is invalid.")
	}
	if enabled && requiresSecret(d.Type) {
		return access.Invalid("secret", "This destination type requires a credential secret object.")
	}
	return nil
}

func (s *Server) checkBinding(d destination, secret []byte) error {
	_, err := open(&s.egress, d, secret)
	if errors.Is(err, errMalformedCredential) {
		return access.Invalid("secret", "Use the channel credential object or null.")
	}
	if err != nil {
		return access.Invalid("destination", "The destination, configuration or secret is invalid.")
	}
	return nil
}

func (s *Server) createNotificationDestination(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input destinationInput
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	return create(s, r, destinations, creation[destinationTarget]{projectID: input.ProjectID, fingerprint: input,
		parse: func(*string) (destinationTarget, error) {
			target, err := parseDestination(input)
			if err != nil {
				return destinationTarget{}, err
			}
			if err := s.checkCreated(target); err != nil {
				return destinationTarget{}, err
			}
			return target, nil
		},
		insert: func(tx pgx.Tx, p access.Principal, projectID *string, target destinationTarget, id, etag string) error {
			if _, err := tx.Exec(r.Context(), createChannelDestinationSQL,
				id, target.name, target.URL, projectID, etag, p.UserID(), target.enabled, target.Type, target.Configuration); err != nil {
				return err
			}
			return s.storeSecretChange(r, tx, id, target.secret)
		}})
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
		target, err := patchDestination(current, patch)
		if err != nil {
			return err
		}
		if _, replaced := target.secret.(newSecret); !replaced && current.SecretConfigured && target.URL != current.URL {
			return access.Invalid("secret", "Changing the destination URL requires a replacement secret or null.")
		}
		secret, err := s.effectiveSecret(r, tx, row.id, current, target.secret)
		if err != nil {
			return err
		}
		if err := s.checkPatched(target, secret); err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), updateChannelDestinationSQL,
			row.id, target.name, target.URL, target.enabled, etag, target.Type, target.Configuration); err != nil {
			return err
		}
		return s.storeSecretChange(r, tx, row.id, target.secret)
	})
}

func patchDestination(current storedDestination, patch map[string]json.RawMessage) (destinationTarget, error) {
	name, err := patched(patch, "name", current.Name, "Use a non-empty destination name.")
	if err != nil {
		return destinationTarget{}, err
	}
	if err := access.ValidText("name", name, nameLimit); err != nil {
		return destinationTarget{}, err
	}
	url, err := patched(patch, "url", current.URL, "Use a valid destination URL.")
	if err != nil {
		return destinationTarget{}, err
	}
	if raw, present := patch["type"]; present && !sameType(raw, current.Type) {
		return destinationTarget{}, access.Invalid("type", "The destination type cannot be changed.")
	}
	configuration, err := patchedConfiguration(patch, current.Configuration)
	if err != nil {
		return destinationTarget{}, err
	}
	enabled, err := patched(patch, "enabled", current.Enabled, "Use true or false.")
	if err != nil {
		return destinationTarget{}, err
	}
	secret, err := destinationSecret(current.Type, patch["secret"])
	if err != nil {
		return destinationTarget{}, err
	}
	return destinationTarget{
		destination: destination{Type: current.Type, URL: strings.TrimSpace(url), Configuration: configuration},
		name:        strings.TrimSpace(name),
		enabled:     enabled,
		secret:      secret,
	}, nil
}

func patched[T any](patch map[string]json.RawMessage, field string, stored T, message string) (T, error) {
	raw, present := patch[field]
	if !present || string(raw) == "null" {
		return stored, nil
	}
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		return stored, access.Invalid(field, message)
	}
	return value, nil
}

func sameType(raw json.RawMessage, current contract.NotificationDestinationType) bool {
	var typ contract.NotificationDestinationType
	return json.Unmarshal(raw, &typ) == nil && typ == current
}

func patchedConfiguration(patch, stored map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	raw, present := patch["configuration"]
	if !present {
		return stored, nil
	}
	return configurationObject(raw)
}

func (s *Server) checkCreated(target destinationTarget) error {
	if replaced, ok := target.secret.(newSecret); ok && len(replaced.value) != 0 {
		return s.checkBinding(target.destination, replaced.value)
	}
	return s.checkEndpoint(target.destination, target.enabled)
}

func (s *Server) checkPatched(target destinationTarget, secret []byte) error {
	if _, replaced := target.secret.(newSecret); len(secret) != 0 || !replaced && !requiresSecret(target.Type) {
		return s.checkBinding(target.destination, secret)
	}
	return s.checkEndpoint(target.destination, target.enabled)
}

func (s *Server) effectiveSecret(r *http.Request, tx pgx.Tx, id string, current storedDestination, change secretChange) ([]byte, error) {
	if replaced, ok := change.(newSecret); ok {
		return replaced.value, nil
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

func (s *Server) storeSecretChange(r *http.Request, tx pgx.Tx, id string, change secretChange) error {
	replaced, ok := change.(newSecret)
	if !ok {
		return nil
	}
	return s.storeNotificationSecret(r, tx, id, replaced.value)
}
