package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
)

// RevisionModel is the published shape of one enabled model inside a provider
// revision. Provider activation writes it; publication reads it.
type RevisionModel struct {
	ID            string               `json:"id"`
	UpstreamModel string               `json:"upstream_model"`
	DisplayName   string               `json:"display_name"`
	Capabilities  []RevisionCapability `json:"capabilities"`
}

// RevisionCapability records whether a tuple was certified when activated.
type RevisionCapability struct {
	Operation   string     `json:"operation"`
	Surface     string     `json:"surface"`
	Mode        string     `json:"mode"`
	Source      string     `json:"source"`
	CertifiedAt *time.Time `json:"certified_at,omitempty"`
}

// RevisionSlot is the published shape of one credential slot.
type RevisionSlot struct {
	Slot
	Default bool `json:"default"`
}

// Configuration is the subset of a provider configuration the gateway needs.
type Configuration struct {
	ProfileID       string `json:"profile_id,omitempty"`
	ProfileRevision string `json:"profile_revision,omitempty"`
	Kind            string `json:"kind"`
	AuthMode        string `json:"auth_mode"`
	Endpoint        string `json:"endpoint"`
	CloudRegion     string `json:"cloud_region"`
	CloudProject    string `json:"cloud_project"`
	Deployment      string `json:"deployment"`
	APIVersion      string `json:"api_version"`
	Options         struct {
		Network           *egress.ConnectionOptions        `json:"network,omitempty"`
		SemanticHeaders   map[string]string                `json:"semantic_headers,omitempty"`
		QuerySettings     map[string]string                `json:"query_settings,omitempty"`
		OperationDefaults map[string]connectors.DefaultSet `json:"operation_defaults,omitempty"`
		Bindings          map[string]connectors.Binding    `json:"bindings,omitempty"`
		Models            map[string]json.RawMessage       `json:"models"`
		CredentialHeaders []string                         `json:"credential_headers"`
		Limits            *Limits                          `json:"limits"`
		ParameterDefaults map[string]json.RawMessage       `json:"parameter_defaults"`
		VendorID          string                           `json:"vendor_id"`
	} `json:"options"`
}

// Published identifies a recorded release.
type Published struct {
	ID       string `json:"id"`
	Sequence int64  `json:"sequence"`
}

// Publish compiles the serving snapshot from active provider revisions and
// published routes, validates it, and records it as the next release inside
// the caller's transaction. The caller holds the installation row lock, so
// sequences are gapless and publication is atomic with the activation that
// caused it. Republishing unchanged configuration is harmless: it records a
// new sequence with the same digest.
func Publish(ctx context.Context, tx pgx.Tx, actor string) (Published, error) {
	snapshot, err := Compile(ctx, tx)
	if err != nil {
		return Published{}, err
	}
	var sequence int64
	if err = tx.QueryRow(ctx, "UPDATE olp_go.installation SET release_sequence=release_sequence+1 WHERE singleton RETURNING release_sequence").Scan(&sequence); err != nil {
		return Published{}, err
	}
	now := time.Now().UTC()
	snapshot.Generation = Generation{ID: uuid.Must(uuid.NewV7()).String(), Ordinal: sequence, ActivatedAt: now}
	if err = snapshot.Validate(); err != nil {
		return Published{}, fmt.Errorf("release rejected: %w", err)
	}
	digest, err := snapshot.Digest()
	if err != nil {
		return Published{}, err
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return Published{}, err
	}
	p := Published{ID: snapshot.Generation.ID, Sequence: sequence}
	_, err = tx.Exec(ctx, "INSERT INTO olp_go.runtime_releases(id,sequence,sha256,snapshot,created_by,created_at,published_at) VALUES($1,$2,$3,$4,$5,$6,$6)", p.ID, sequence, digest, encoded, actor, now)
	return p, err
}

// Compile reads the serving configuration without recording a release. Draft
// state never appears here: providers serve their active revision and routes
// their latest published revision.
func Compile(ctx context.Context, tx pgx.Tx) (*Snapshot, error) {
	snapshot := &Snapshot{Providers: map[string]Provider{}, Routes: map[string]Route{}}
	rows, err := tx.Query(ctx, "SELECT p.id::text,p.state,r.id::text,r.name,r.configuration,r.models,r.slots,p.project_id::text FROM olp_go.providers p JOIN olp_go.provider_revisions r ON r.id=p.active_revision_id WHERE p.state IN ('active','disabled')")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var state string
		var configuration, models, slots []byte
		provider := Provider{Capabilities: []Capability{}}
		if err = rows.Scan(&provider.ID, &state, &provider.RevisionID, &provider.Name, &configuration, &models, &slots, &provider.ProjectID); err != nil {
			rows.Close()
			return nil, err
		}
		var cfg Configuration
		var revisionModels []RevisionModel
		var revisionSlots []RevisionSlot
		if err = json.Unmarshal(configuration, &cfg); err == nil {
			err = json.Unmarshal(models, &revisionModels)
		}
		if err == nil {
			err = json.Unmarshal(slots, &revisionSlots)
		}
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("provider %s revision: %w", provider.ID, err)
		}
		provider.Enabled = state == "active"
		provider.Network = cfg.Options.Network
		provider.ProfileID, provider.ProfileRevision = cfg.ProfileID, cfg.ProfileRevision
		provider.SemanticHeaders, provider.QuerySettings = cfg.Options.SemanticHeaders, cfg.Options.QuerySettings
		provider.OperationDefaults, provider.Bindings = cfg.Options.OperationDefaults, cfg.Options.Bindings
		provider.Kind = cfg.Kind
		provider.AuthMode = cfg.AuthMode
		provider.Endpoint = cfg.Endpoint
		provider.CloudRegion, provider.CloudProject, provider.Deployment, provider.APIVersion = cfg.CloudRegion, cfg.CloudProject, cfg.Deployment, cfg.APIVersion
		provider.Models = cfg.Options.Models
		provider.CredentialHeaders = cfg.Options.CredentialHeaders
		provider.ParameterDefaults = cfg.Options.ParameterDefaults
		provider.VendorID = cfg.Options.VendorID
		provider.Limits = publishedLimits(cfg.Options.Limits)
		for _, model := range revisionModels {
			for _, c := range model.Capabilities {
				if c.Source == "certified" {
					provider.Capabilities = append(provider.Capabilities, Capability{Model: model.UpstreamModel, Operation: c.Operation, Surface: c.Surface, Mode: c.Mode})
				}
			}
		}
		for _, slot := range revisionSlots {
			provider.Slots = append(provider.Slots, slot.Slot)
			if slot.Default {
				provider.ActiveCredential = slot.CredentialID
				provider.DefaultSlotID = slot.ID
			}
		}
		snapshot.Providers[provider.ID] = provider
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows, err = tx.Query(ctx, "SELECT r.id::text,r.slug,v.id::text,v.revision,v.operations,v.overall_timeout_ms,v.max_attempts,v.targets,v.activated_at,v.routing_policy,r.project_id::text,v.content_policy,v.fidelity FROM olp_go.routes r JOIN olp_go.route_revisions v ON v.id=r.latest_revision_id WHERE r.state='active'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var operations, targets, policy, contentPolicy, fidelity []byte
		route := Route{}
		if err = rows.Scan(&route.ID, &route.Slug, &route.RevisionID, &route.Revision, &operations, &route.OverallTimeout, &route.MaxAttempts, &targets, &route.PublishedAt, &policy, &route.ProjectID, &contentPolicy, &fidelity); err != nil {
			return nil, err
		}
		var published []PublishedTarget
		if err = json.Unmarshal(operations, &route.Operations); err == nil {
			err = json.Unmarshal(targets, &published)
		}
		if err != nil {
			return nil, fmt.Errorf("route %s revision: %w", route.Slug, err)
		}
		if len(policy) > 0 {
			if err = json.Unmarshal(policy, &route.Policy); err != nil {
				return nil, err
			}
		}
		if len(contentPolicy) > 0 {
			if err = json.Unmarshal(contentPolicy, &route.ContentPolicy); err != nil {
				return nil, fmt.Errorf("route %s content policy: %w", route.Slug, err)
			}
		}
		route.Fidelity, err = DecodeFidelity(fidelity)
		if err != nil {
			return nil, fmt.Errorf("route %s fidelity: %w", route.Slug, err)
		}
		route.RoutingID = route.ID
		route.PublishedAt = route.PublishedAt.UTC()
		for _, t := range published {
			route.Targets = append(route.Targets, Target{ID: t.ID, ProviderID: t.ProviderID, ProviderModel: t.ProviderModel, Priority: t.Priority, Weight: t.Weight, Timeout: t.TimeoutMS, RoutingID: t.ProviderModelID})
		}
		snapshot.Routes[route.Slug] = route
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows, err = tx.Query(ctx, "SELECT scope,scope_id::text,policy FROM olp_go.routing_policies WHERE scope IN ('installation','api-key')")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var scope, id string
		var raw []byte
		if err = rows.Scan(&scope, &id, &raw); err != nil {
			return nil, err
		}
		var p Policy
		if err = json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		if scope == "installation" {
			snapshot.InstallationPolicy = &p
		} else {
			if snapshot.KeyPolicies == nil {
				snapshot.KeyPolicies = map[string]*Policy{}
			}
			snapshot.KeyPolicies[id] = &p
		}
	}
	return snapshot, rows.Err()
}

// publishedLimits drops a connection quota that bounds nothing so clearing
// every limit publishes the same snapshot as never setting one.
func publishedLimits(l *Limits) *Limits {
	if l == nil || (l.RequestsPerMinute == nil && l.TokensPerMinute == nil && l.MaxConcurrency == nil) {
		return nil
	}
	return l
}

// PublishedTarget is the stored shape of one route revision target. The
// provider model identity doubles as the routing identity so affinity survives
// revisions that keep the same target.
type PublishedTarget struct {
	ID              string `json:"id"`
	ProviderModelID string `json:"provider_model_id"`
	ProviderID      string `json:"provider_id"`
	ProviderName    string `json:"provider_name"`
	ProviderModel   string `json:"provider_model"`
	Priority        int    `json:"priority"`
	Weight          int64  `json:"weight"`
	TimeoutMS       int64  `json:"timeout_ms"`
	Position        int    `json:"position"`
}
