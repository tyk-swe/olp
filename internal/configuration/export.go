package configuration

import (
	"context"
	"encoding/json"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

func (s *Server) exportDocument(ctx context.Context, q access.Queryer) (*Document, error) {
	doc := &Document{APIVersion: APIVersion}
	projects, err := q.Query(ctx, "SELECT name FROM olp_go.projects ORDER BY lower(name),name")
	if err != nil {
		return nil, err
	}
	defer projects.Close()
	for projects.Next() {
		var name string
		if err = projects.Scan(&name); err != nil {
			return nil, err
		}
		doc.Projects = append(doc.Projects, ProjectEntry{Name: name})
	}
	if err = projects.Err(); err != nil {
		return nil, err
	}
	providersRows, err := q.Query(ctx, `SELECT p.id::text,p.name,pr.name,p.state,r.configuration,r.models,r.slots
        FROM olp_go.providers p LEFT JOIN olp_go.projects pr ON pr.id=p.project_id
        LEFT JOIN olp_go.provider_revisions r ON r.id=p.active_revision_id AND p.state='active'
        ORDER BY lower(p.name),p.name`)
	if err != nil {
		return nil, err
	}
	defer providersRows.Close()
	type pendingProvider struct {
		entry *ProviderEntry
		id    string
		live  bool
	}
	var pending []pendingProvider
	for providersRows.Next() {
		var id, name, state string
		var projectName *string
		var configuration, models, slots []byte
		if err = providersRows.Scan(&id, &name, &projectName, &state, &configuration, &models, &slots); err != nil {
			return nil, err
		}
		entry := ProviderEntry{Name: name, Project: projectName}
		if configuration != nil {
			if err = json.Unmarshal(configuration, &entry.Configuration); err != nil {
				return nil, err
			}
			var revisionModels []runtime.RevisionModel
			if err = json.Unmarshal(models, &revisionModels); err != nil {
				return nil, err
			}
			for _, m := range revisionModels {
				model := ModelEntry{UpstreamModel: m.UpstreamModel, DisplayName: m.DisplayName, Enabled: true}
				for _, c := range m.Capabilities {
					model.Capabilities = append(model.Capabilities, CapabilityEntry{Operation: c.Operation, Surface: c.Surface, Mode: c.Mode})
				}
				entry.Models = append(entry.Models, model)
			}
			var revisionSlots []runtime.RevisionSlot
			if err = json.Unmarshal(slots, &revisionSlots); err != nil {
				return nil, err
			}
			for i, rs := range revisionSlots {
				slot := SlotEntry{Name: rs.Name, IsDefault: rs.Default, Position: i, Enabled: rs.Enabled, Priority: rs.Priority, Weight: rs.Weight,
					Restrictions: Restrictions{AllowedAPIKeys: []string{}, AllowedModels: orEmpty(rs.AllowedModels), AllowedRoutes: orEmpty(rs.AllowedRoutes)},
					Limits:       providers.Limits{MaxConcurrency: rs.MaxConcurrency, RequestsPerMinute: rs.RequestsPerMinute, TokensPerMinute: rs.TokensPerMinute}}
				if rs.CredentialID != nil {
					ref := CredentialRef(name, rs.Name)
					slot.CredentialRef = &ref
				}
				entry.Slots = append(entry.Slots, slot)
			}
			portableNetwork(&entry)
			doc.Providers = append(doc.Providers, entry)
			continue
		}
		pending = append(pending, pendingProvider{entry: &entry, id: id})
	}
	if err = providersRows.Err(); err != nil {
		return nil, err
	}
	for _, pp := range pending {
		var configuration []byte
		if err = q.QueryRow(ctx, "SELECT configuration FROM olp_go.providers WHERE id=$1", pp.id).Scan(&configuration); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(configuration, &pp.entry.Configuration); err != nil {
			return nil, err
		}
		models, err := exportModels(ctx, q, pp.id)
		if err != nil {
			return nil, err
		}
		pp.entry.Models = models
		slots, err := exportSlots(ctx, q, pp.id, pp.entry.Name)
		if err != nil {
			return nil, err
		}
		pp.entry.Slots = slots
		portableNetwork(pp.entry)
		doc.Providers = append(doc.Providers, *pp.entry)
	}
	routes, err := q.Query(ctx, `SELECT r.slug,pr.name,r.state='retired',v.operations,v.overall_timeout_ms,v.max_attempts,v.targets,v.routing_policy,v.content_policy
        FROM olp_go.routes r JOIN olp_go.route_revisions v ON v.id=r.latest_revision_id
        LEFT JOIN olp_go.projects pr ON pr.id=r.project_id ORDER BY r.slug`)
	if err != nil {
		return nil, err
	}
	defer routes.Close()
	for routes.Next() {
		var route RouteEntry
		var operations, targets, policy []byte
		if err = routes.Scan(&route.Slug, &route.Project, &route.Retired, &operations, &route.OverallTimeoutMS, &route.MaxAttempts, &targets, &policy, &route.ContentPolicy); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(operations, &route.Operations); err != nil {
			return nil, err
		}
		var published []runtime.PublishedTarget
		if err = json.Unmarshal(targets, &published); err != nil {
			return nil, err
		}
		for _, t := range published {
			route.Targets = append(route.Targets, TargetEntry{Provider: t.ProviderName, ProviderModel: t.ProviderModel, Priority: t.Priority, Weight: t.Weight, TimeoutMS: int(t.TimeoutMS)})
		}
		if policy != nil {
			var p runtime.Policy
			if err = json.Unmarshal(policy, &p); err != nil {
				return nil, err
			}
			route.RoutingPolicy = &p
		}
		doc.Routes = append(doc.Routes, route)
	}
	if err = routes.Err(); err != nil {
		return nil, err
	}
	revisions, _, err := usage.ListRevisions(ctx, q, nil, 1)
	if err != nil {
		return nil, err
	}
	if len(revisions) == 1 {
		providerNames, err := providerNameMap(ctx, q)
		if err != nil {
			return nil, err
		}
		latest := revisions[0]
		doc.Pricing = &PricingEntry{EffectiveAt: latest.EffectiveAt.Format(time.RFC3339)}
		for _, price := range latest.Prices {
			entry := PriceEntry{VendorID: price.VendorID, ProviderKind: price.ProviderKind, Model: price.Model, Operation: price.Operation,
				InputPerMillion: price.InputPerMillion, CachedInputPerMillion: price.CachedInputPerMillion,
				OutputPerMillion: price.OutputPerMillion, CacheWriteInputPerMillion: price.CacheWriteInputPerMillion,
				CacheWrite5MInputPerMillion: price.CacheWrite5MInputPerMillion,
				CacheWrite1HInputPerMillion: price.CacheWrite1HInputPerMillion,
				UnitPrice:                   price.UnitPrice, Currency: price.Currency}
			if price.ProviderID != nil {
				name := providerNames[*price.ProviderID]
				entry.Provider = &name
			}
			doc.Pricing.Prices = append(doc.Pricing.Prices, entry)
		}
	}
	doc.canonicalize()
	return doc, nil
}

func exportModels(ctx context.Context, q access.Queryer, providerID string) ([]ModelEntry, error) {
	rows, err := q.Query(ctx, "SELECT upstream_model,display_name,enabled,capabilities FROM olp_go.provider_models WHERE provider_id=$1 ORDER BY upstream_model", providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var models []ModelEntry
	for rows.Next() {
		var m ModelEntry
		var capabilities []byte
		if err = rows.Scan(&m.UpstreamModel, &m.DisplayName, &m.Enabled, &capabilities); err != nil {
			return nil, err
		}
		var stored []struct {
			Operation string `json:"operation"`
			Surface   string `json:"surface"`
			Mode      string `json:"mode"`
		}
		if err = json.Unmarshal(capabilities, &stored); err != nil {
			return nil, err
		}
		for _, c := range stored {
			m.Capabilities = append(m.Capabilities, CapabilityEntry{Operation: c.Operation, Surface: c.Surface, Mode: c.Mode})
		}
		models = append(models, m)
	}
	return models, rows.Err()
}

func exportSlots(ctx context.Context, q access.Queryer, providerID, providerName string) ([]SlotEntry, error) {
	rows, err := q.Query(ctx, "SELECT name,is_default,position,enabled,priority,weight,credential_id::text,restrictions,limits FROM olp_go.provider_slots WHERE provider_id=$1 ORDER BY position", providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var slots []SlotEntry
	for rows.Next() {
		var s SlotEntry
		var credentialID *string
		var restrictions, limits []byte
		if err = rows.Scan(&s.Name, &s.IsDefault, &s.Position, &s.Enabled, &s.Priority, &s.Weight, &credentialID, &restrictions, &limits); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(restrictions, &s.Restrictions); err != nil {
			return nil, err
		}
		s.Restrictions.AllowedAPIKeys = []string{}
		if err = json.Unmarshal(limits, &s.Limits); err != nil {
			return nil, err
		}
		if credentialID != nil {
			ref := CredentialRef(providerName, s.Name)
			s.CredentialRef = &ref
		}
		slots = append(slots, s)
	}
	return slots, rows.Err()
}

func providerNameMap(ctx context.Context, q access.Queryer) (map[string]string, error) {
	rows, err := q.Query(ctx, "SELECT id::text,name FROM olp_go.providers")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := map[string]string{}
	for rows.Next() {
		var id, name string
		if err = rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		names[id] = name
	}
	return names, rows.Err()
}

func orEmpty(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
