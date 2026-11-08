package sinks

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/tyk-swe/olp/internal/promotion"
	"reflect"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secrets"
)

func sinkReference(entry promotion.SinkEntry) string {
	encoded, _ := json.Marshal(promotion.SinkKey(entry))
	sum := sha256.Sum256(encoded)
	return "sink/" + hex.EncodeToString(sum[:])
}
func ValidateAndNormalizeSinks(s *promotion.Server, doc *promotion.Document) error {
	if len(doc.Sinks) > 64 {
		return access.Invalid("sinks", "Declare at most 64 export sinks.")
	}
	seen := map[[2]string]bool{}
	for i := range doc.Sinks {
		e := &doc.Sinks[i]
		e.Name = strings.TrimSpace(e.Name)
		if e.Project != nil {
			value := strings.TrimSpace(*e.Project)
			e.Project = &value
			if !promotion.GuardrailLabel(value) {
				return access.Invalid("sinks.project", "Use an owning project name.")
			}
		}
		key := promotion.SinkKey(*e)
		if seen[key] {
			return access.Invalid("sinks", "Sink names must be unique inside each scope.")
		}
		seen[key] = true
		d, err := NormalizeSpec(Spec{Name: e.Name, ProjectID: e.Project, Type: e.Type, Destination: e.Destination, Streams: e.Streams, Enabled: e.Enabled}, s.Egress)
		if err != nil {
			return err
		}
		e.Name, e.Destination, e.Streams = d.Name, d.Destination, slices.Sorted(slices.Values(d.Streams))
		if e.CredentialRef != nil && *e.CredentialRef != sinkReference(*e) {
			return access.Invalid("sinks.credential_ref", "Use the deterministic exported signing-credential reference.")
		}
	}
	return nil
}
func ExportSinks(ctx context.Context, q access.Queryer) ([]promotion.SinkEntry, error) {
	rows, err := q.Query(ctx, "SELECT s.name,p.name,s.type,s.destination,s.streams,s.enabled,s.credential_id IS NOT NULL FROM olp.managed_export_sinks s LEFT JOIN olp.projects p ON p.id=s.project_id WHERE s.retired_at IS NULL")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []promotion.SinkEntry{}
	for rows.Next() {
		var e promotion.SinkEntry
		var credential bool
		if err = rows.Scan(&e.Name, &e.Project, &e.Type, &e.Destination, &e.Streams, &e.Enabled, &credential); err != nil {
			return nil, err
		}
		if credential {
			value := sinkReference(e)
			e.CredentialRef = &value
		}
		slices.Sort(e.Streams)
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

type existingSink struct {
	id         string
	credential *string
	entry      promotion.SinkEntry
}

func readSink(ctx context.Context, q access.Queryer, entry promotion.SinkEntry) (*existingSink, error) {
	var current existingSink
	err := q.QueryRow(ctx, "SELECT s.id::text,s.credential_id::text,s.name,p.name,s.type,s.destination,s.streams,s.enabled FROM olp.managed_export_sinks s LEFT JOIN olp.projects p ON p.id=s.project_id WHERE s.retired_at IS NULL AND lower(s.name)=lower($1) AND coalesce(lower(p.name),'')=$2", entry.Name, promotion.SinkKey(entry)[0]).Scan(&current.id, &current.credential, &current.entry.Name, &current.entry.Project, &current.entry.Type, &current.entry.Destination, &current.entry.Streams, &current.entry.Enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if current.credential != nil {
		value := sinkReference(current.entry)
		current.entry.CredentialRef = &value
	}
	slices.Sort(current.entry.Streams)
	return &current, nil
}
func SinkBindingMatches(s *promotion.Server, ctx context.Context, q access.Queryer, id string, binding promotion.CredentialBinding) (bool, error) {
	value, err := s.Access.Keys.Read(ctx, q, s.Access.Installation, id, secrets.SinkCredential)
	if err != nil {
		return false, err
	}
	defer clear(value)
	return subtle.ConstantTimeCompare(value, []byte(binding.Secret)) == 1, nil
}
func PlanSinks(s *promotion.Server, ctx context.Context, q access.Queryer, doc *promotion.Document, bindings promotion.BindingSet, result *promotion.PlanResult) error {
	active := 0
	if len(doc.Sinks) > 0 {
		if err := q.QueryRow(ctx, "SELECT count(*) FROM olp.managed_export_sinks WHERE retired_at IS NULL").Scan(&active); err != nil {
			return err
		}
	}
	for _, e := range doc.Sinks {
		if e.Project != nil {
			var present bool
			if err := q.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.projects WHERE lower(name)=lower($1))", *e.Project).Scan(&present); err != nil {
				return err
			}
			for _, declared := range doc.Projects {
				present = present || strings.EqualFold(declared.Name, *e.Project)
			}
			if !present {
				result.Conflict("sink", e.Name, "The owning project is not declared or present at the destination.")
				continue
			}
		}
		current, err := readSink(ctx, q, e)
		if err != nil {
			return err
		}
		if e.CredentialRef != nil {
			if _, bound := bindings[*e.CredentialRef]; !bound && (current == nil || current.credential == nil) {
				result.Blocker("sink", e.Name, "Supply the destination signing-credential binding.")
			}
		}
		if current == nil {
			active++
			if active > 64 {
				result.Blocker("sink", e.Name, "The destination would exceed 64 active sinks.")
			}
		}
		action := "create"
		if current != nil {
			action = "replace"
			if reflect.DeepEqual(current.entry, e) {
				action = "reuse"
			}
			if e.CredentialRef != nil {
				if binding, ok := bindings[*e.CredentialRef]; ok && current.credential != nil {
					equal, err := SinkBindingMatches(s, ctx, q, *current.credential, binding)
					if err != nil {
						return err
					}
					if !equal {
						action = "replace"
					}
				}
			}
		}
		result.Add("sink", e.Name, action, "Export content-free durable facts; queued events and delivery history remain local.")
	}
	return nil
}
func sinkOperation(e promotion.SinkEntry) access.Operation {
	if e.Project == nil {
		return access.Settings
	}
	return access.Keys
}
func AuthorizeSinkPromotion(p access.Principal, doc *promotion.Document) error {
	for _, e := range doc.Sinks {
		if err := p.Authorize(sinkOperation(e)); err != nil {
			return err
		}
	}
	return nil
}
func ApplySinks(s *promotion.Server, ctx context.Context, tx pgx.Tx, p access.Principal, doc *promotion.Document, bindings promotion.BindingSet) error {
	if err := AuthorizeSinkPromotion(p, doc); err != nil {
		return err
	}
	for _, e := range doc.Sinks {
		var projectID *string
		if e.Project != nil {
			var id string
			if err := tx.QueryRow(ctx, "SELECT id::text FROM olp.projects WHERE lower(name)=lower($1)", *e.Project).Scan(&id); err != nil {
				return err
			}
			projectID = &id
			if err := p.Project(projectID, access.Change); err != nil {
				return err
			}
		}
		current, err := readSink(ctx, tx, e)
		if err != nil {
			return err
		}
		binding, bound := promotion.CredentialBinding{}, false
		if e.CredentialRef != nil {
			binding, bound = bindings[*e.CredentialRef]
		}
		var old, next *string
		if current != nil {
			old = current.credential
			next = old
		}
		if e.CredentialRef == nil {
			next = nil
		} else if bound {
			equal := false
			if old != nil {
				equal, err = SinkBindingMatches(s, ctx, tx, *old, binding)
				if err != nil {
					return err
				}
			}
			if !equal {
				id := access.NewID()
				material := []byte(binding.Secret)
				err = s.Access.Keys.Store(ctx, tx, s.Access.Installation, id, secrets.SinkCredential, material, nil)
				clear(material)
				if err != nil {
					return err
				}
				next = &id
			}
		}
		if current != nil && reflect.DeepEqual(current.entry, e) && reflect.DeepEqual(old, next) {
			continue
		}
		etag := access.NewID()
		if current == nil {
			_, err = tx.Exec(ctx, "INSERT INTO olp.managed_export_sinks(id,project_id,name,type,destination,streams,enabled,credential_id,etag,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)", access.NewID(), projectID, e.Name, e.Type, e.Destination, e.Streams, e.Enabled, next, etag, p.UserID())
		} else {
			_, err = tx.Exec(ctx, "UPDATE olp.managed_export_sinks SET name=$2,destination=$3,streams=$4,enabled=$5,credential_id=$6,etag=$7 WHERE id=$1", current.id, e.Name, e.Destination, e.Streams, e.Enabled, next, etag)
		}
		if err != nil {
			return err
		}
		if old != nil && !reflect.DeepEqual(old, next) {
			if _, err = tx.Exec(ctx, "DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2", *old, secrets.SinkCredential); err != nil {
				return err
			}
		}
	}
	return nil
}
