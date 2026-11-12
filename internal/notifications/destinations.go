package notifications

import (
	"cmp"
	"context"
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
	Configuration    json.RawMessage                      `json:"configuration"`
	Enabled          bool                                 `json:"enabled"`
	SecretConfigured bool                                 `json:"secret_configured"`
}

type secretChange interface{ secretChange() }

type keptSecret struct{}

type newSecret struct{ value []byte }

func (keptSecret) secretChange() {}

func (newSecret) secretChange() {}

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

func configurationObject(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage(`{}`), nil
	}
	if len(raw) > configurationLimit {
		return nil, access.Invalid("configuration", "Use a configuration object of at most 4096 bytes.")
	}
	if _, err := decodeObject(raw); err != nil {
		return nil, access.Invalid("configuration", "Use a configuration object.")
	}
	return raw, nil
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
	if err := kindOf(d.Type).check(&s.egress, d); err != nil {
		return access.Invalid("destination", "The destination or its configuration is invalid.")
	}
	if enabled && requiresSecret(d.Type) {
		return access.Invalid("secret", "This destination type requires a credential secret object.")
	}
	return nil
}

func (s *Server) checkBinding(d destination, secret []byte) error {
	_, err := kindOf(d.Type).open(&s.egress, d, secret)
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
			return s.storeSecretChange(r.Context(), tx, id, target.secret)
		}})
}

func (s *Server) updateNotificationDestination(r *http.Request, _ access.Principal) (access.Reply, error) {
	patch, err := decodePatch(r, "destination", "Unknown destination field.", destinationPatchFields)
	if err != nil {
		return access.Reply{}, err
	}
	return s.update(r, destinations, func(tx pgx.Tx, row document, etag string) error {
		var current storedDestination
		if err := json.Unmarshal(row.body, &current); err != nil {
			return err
		}
		target, err := patchDestination(current, patch)
		if err != nil {
			return err
		}
		if _, replaced := target.secret.(newSecret); !replaced && current.SecretConfigured && target.URL != current.URL {
			return access.Invalid("secret", "Changing the destination URL requires a replacement secret or null.")
		}
		secret, err := s.effectiveSecret(r.Context(), tx, row.id, current, target.secret)
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
		return s.storeSecretChange(r.Context(), tx, row.id, target.secret)
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

func patchedConfiguration(patch map[string]json.RawMessage, stored json.RawMessage) (json.RawMessage, error) {
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
	_, replaced := target.secret.(newSecret)
	switch {
	case len(secret) != 0:
		return s.checkBinding(target.destination, secret)
	case replaced || requiresSecret(target.Type):
		return s.checkEndpoint(target.destination, target.enabled)
	default:
		return s.checkBinding(target.destination, nil)
	}
}

func (s *Server) effectiveSecret(ctx context.Context, tx pgx.Tx, id string, current storedDestination, change secretChange) ([]byte, error) {
	if replaced, ok := change.(newSecret); ok {
		return replaced.value, nil
	}
	if !current.SecretConfigured {
		return nil, nil
	}
	var secretID *string
	if err := tx.QueryRow(ctx, readDestinationSecretSQL, id).Scan(&secretID); err != nil || secretID == nil {
		return nil, err
	}
	return s.access.Keys.Read(ctx, tx, s.access.Installation, *secretID, secrets.NotificationSecret)
}

const (
	configurationLimit = 4096
	signingSecretLimit = 1024
	secretObjectLimit  = 16384
)

var destinations = resource{
	parameter: "notification_destination_id", kind: "notification_destination", path: "/api/v1/notifications/destinations/",
	documentsSQL: destinationDocumentsSQL, lockSQL: lockDestinationSQL, remove: removeDestination,
}

func removeDestination(ctx context.Context, tx pgx.Tx, id string) error {
	var secretID *string
	if err := tx.QueryRow(ctx, deleteDestinationSQL, id).Scan(&secretID); err != nil || secretID == nil {
		return err
	}
	return deleteSecret(ctx, tx, *secretID)
}

func deleteSecret(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, "DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2", id, secrets.NotificationSecret)
	return err
}

func (s *Server) storeSecretChange(ctx context.Context, tx pgx.Tx, id string, change secretChange) error {
	replaced, ok := change.(newSecret)
	if !ok {
		return nil
	}
	secretID, err := s.storeSecret(ctx, tx, replaced.value)
	if err != nil {
		return err
	}
	var previous *string
	if err := tx.QueryRow(ctx, readDestinationSecretSQL, id).Scan(&previous); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE olp.notification_destinations SET secret_id=$2 WHERE id=$1", id, secretID); err != nil {
		return err
	}
	if previous == nil {
		return nil
	}
	return deleteSecret(ctx, tx, *previous)
}

func (s *Server) storeSecret(ctx context.Context, tx pgx.Tx, secret []byte) (*string, error) {
	if secret == nil {
		return nil, nil
	}
	id := access.NewID()
	if err := s.access.Keys.Store(ctx, tx, s.access.Installation, id, secrets.NotificationSecret, secret, nil); err != nil {
		return nil, err
	}
	return &id, nil
}

const destinationDocumentsSQL = `SELECT d.id,d.project_id,d.etag,jsonb_build_object(
	'id',d.id,'name',d.name,'url',d.url,'type',d.type,'configuration',d.configuration,'secret_configured',d.secret_id IS NOT NULL,
	'project_id',d.project_id,'project_name',p.name,'enabled',d.enabled,'etag',d.etag,
	'created_by',d.created_by,'created_by_email',u.email,'created_at',d.created_at,'updated_at',d.updated_at) AS document
 FROM olp.notification_destinations d
 JOIN olp.users u ON u.id=d.created_by
 LEFT JOIN olp.projects p ON p.id=d.project_id`

const lockDestinationSQL = `SELECT etag::text,project_id::text FROM olp.notification_destinations WHERE id=$1 FOR UPDATE`

const createChannelDestinationSQL = `INSERT INTO olp.notification_destinations(id,name,url,project_id,etag,created_by,enabled,type,configuration)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`

const updateChannelDestinationSQL = `UPDATE olp.notification_destinations SET name=$2,url=$3,enabled=$4,etag=$5,type=$6,configuration=$7,updated_at=now() WHERE id=$1`

const readDestinationSecretSQL = `SELECT secret_id::text FROM olp.notification_destinations WHERE id=$1`

const deleteDestinationSQL = `DELETE FROM olp.notification_destinations WHERE id=$1 RETURNING secret_id::text`
