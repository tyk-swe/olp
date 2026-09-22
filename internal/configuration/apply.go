package configuration

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/routes"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

type resolvedSlot struct {
	entry        *SlotEntry
	id           string
	credentialID *string
}

func (s *Server) applyDocument(ctx context.Context, tx pgx.Tx, p access.Principal, doc *Document, bindings map[string]string) error {
	state, err := loadState(ctx, tx)
	if err != nil {
		return err
	}
	projectIDs := map[string]string{}
	for _, project := range doc.Projects {
		key := strings.ToLower(project.Name)
		if id, ok := state.projects[key]; ok {
			projectIDs[key] = id
			continue
		}
		id := access.NewID()
		if _, err = tx.Exec(ctx, "INSERT INTO olp_go.projects(id,name,etag,created_by) VALUES($1,$2,$3,$4)", id, project.Name, access.NewID(), p.UserID()); err != nil {
			return err
		}
		projectIDs[key] = id
	}
	providerIDs := map[string]string{}
	for name, existing := range state.providers {
		providerIDs[name] = existing.ID
	}
	for i := range doc.Providers {
		entry := &doc.Providers[i]
		var projectID *string
		if entry.Project != nil {
			id := projectIDs[strings.ToLower(*entry.Project)]
			projectID = &id
		}
		configuration, err := json.Marshal(entry.Configuration)
		if err != nil {
			return err
		}
		existing, ok := state.providers[strings.ToLower(entry.Name)]
		var providerID string
		switch {
		case !ok:
			providerID = access.NewID()
			if _, err = tx.Exec(ctx, "INSERT INTO olp_go.providers(id,name,kind,state,configuration,etag,slots_etag,created_by,project_id) VALUES($1,$2,$3,'draft',$4,$5,$6,$7,$8)", providerID, entry.Name, entry.Configuration.Kind, configuration, access.NewID(), access.NewID(), p.UserID(), projectID); err != nil {
				return err
			}
		default:
			providerID = existing.ID
			current, err := s.currentProviderEntry(ctx, tx, existing, state.projectOf(existing.ProjectID))
			if err != nil {
				return err
			}
			if !canonicalEqualProvider(entry, current) {
				if _, err = tx.Exec(ctx, "UPDATE olp_go.providers SET name=$2,configuration=$3,etag=$4,draft_dirty=true,updated_at=now() WHERE id=$1", providerID, entry.Name, configuration, access.NewID()); err != nil {
					return err
				}
			} else {
				if err = s.bindChangedCredentials(ctx, tx, providerID, entry, existing, bindings); err != nil {
					return err
				}
				if err := s.bindNetwork(ctx, tx, providerID, entry, existing, bindings); err != nil {
					return err
				}
				providerIDs[strings.ToLower(entry.Name)] = providerID
				continue
			}
		}
		if err := s.bindNetwork(ctx, tx, providerID, entry, existing, bindings); err != nil {
			return err
		}
		resolved, err := s.resolveSlots(ctx, tx, providerID, entry, existing, ok, bindings)
		if err != nil {
			return err
		}
		if err = s.replaceDraftContents(ctx, tx, providerID, entry, resolved); err != nil {
			return err
		}
		providerIDs[strings.ToLower(entry.Name)] = providerID
	}
	for i := range doc.Routes {
		desired := doc.Routes[i]
		route := &desired
		var projectID *string
		if route.Project != nil {
			id := projectIDs[strings.ToLower(*route.Project)]
			projectID = &id
		}
		draft, staged := state.drafts[routeKey(route.Slug, route.Project)]
		if staged {
			current, err := s.currentRouteEntry(ctx, tx, draft.ID, route, state)
			if err != nil {
				return err
			}
			if len(route.Fidelity) == 0 {
				route.Fidelity = current.Fidelity
			}
			if canonicalEqualRoute(route, current) {
				continue
			}
		}
		if !staged && len(route.Fidelity) == 0 {
			if existing, ok := state.routes[route.Slug]; ok && lower(state.projectOf(existing.ProjectID)) == lower(route.Project) {
				route.Fidelity = existing.Fidelity
			}
		}
		if err := routes.ValidateFidelityPolicy(route.Fidelity, route.ContentPolicy); err != nil {
			return err
		}
		input := routes.DraftInput{Slug: route.Slug, Operations: route.Operations, OverallTimeoutMS: route.OverallTimeoutMS, MaxAttempts: route.MaxAttempts, ContentPolicy: route.ContentPolicy, Fidelity: route.Fidelity}
		for _, t := range route.Targets {
			providerID := providerIDs[strings.ToLower(t.Provider)]
			input.Targets = append(input.Targets, routes.TargetInput{ProviderID: &providerID, ProviderModel: &t.ProviderModel, Priority: t.Priority, Weight: t.Weight, TimeoutMS: t.TimeoutMS})
		}
		targets, err := routes.ValidateDraftInput(ctx, tx, &input, projectID, nil)
		if err != nil {
			return err
		}
		operations, _ := json.Marshal(input.Operations)
		encoded, _ := json.Marshal(targets)
		var draftID string
		if staged {
			draftID = draft.ID
			if _, err = tx.Exec(ctx, "UPDATE olp_go.route_drafts SET state='draft',operations=$3,overall_timeout_ms=$4,max_attempts=$5,targets=$6,content_policy=$7,etag=$8,fidelity=$9,updated_at=now() WHERE id=$1 AND slug=$2", draftID, input.Slug, operations, input.OverallTimeoutMS, input.MaxAttempts, encoded, input.ContentPolicy, access.NewID(), input.Fidelity); err != nil {
				return err
			}
		} else {
			draftID = access.NewID()
			if _, err = tx.Exec(ctx, "INSERT INTO olp_go.route_drafts(id,slug,state,operations,overall_timeout_ms,max_attempts,targets,content_policy,etag,created_by,project_id,fidelity) VALUES($1,$2,'draft',$3,$4,$5,$6,$7,$8,$9,$10,$11)", draftID, input.Slug, operations, input.OverallTimeoutMS, input.MaxAttempts, encoded, input.ContentPolicy, access.NewID(), p.UserID(), projectID, input.Fidelity); err != nil {
				return err
			}
		}
		if route.RoutingPolicy == nil {
			if _, err = tx.Exec(ctx, "DELETE FROM olp_go.routing_policies WHERE scope='route-draft' AND scope_id=$1", draftID); err != nil {
				return err
			}
		} else {
			policy, _ := json.Marshal(route.RoutingPolicy)
			if _, err = tx.Exec(ctx, "INSERT INTO olp_go.routing_policies(scope,scope_id,policy,etag,updated_by) VALUES('route-draft',$1,$2,$3,$4) ON CONFLICT(scope,scope_id) DO UPDATE SET policy=excluded.policy,etag=excluded.etag,updated_by=excluded.updated_by,updated_at=now()", draftID, policy, access.NewID(), p.UserID()); err != nil {
				return err
			}
		}
	}
	if doc.Pricing != nil {
		changed, err := s.pricingChanged(ctx, tx, doc)
		if err != nil {
			return err
		}
		if changed {
			effective, _ := time.Parse(time.RFC3339, doc.Pricing.EffectiveAt)
			if !effective.After(time.Now()) {
				effective = time.Now().UTC()
			}
			prices := make([]usage.Price, 0, len(doc.Pricing.Prices))
			for _, entry := range doc.Pricing.Prices {
				price := usage.Price{VendorID: entry.VendorID, ProviderKind: entry.ProviderKind, Model: entry.Model, Operation: entry.Operation,
					InputPerMillion: entry.InputPerMillion, CachedInputPerMillion: entry.CachedInputPerMillion,
					OutputPerMillion: entry.OutputPerMillion, CacheWriteInputPerMillion: entry.CacheWriteInputPerMillion,
					CacheWrite5MInputPerMillion: entry.CacheWrite5MInputPerMillion,
					CacheWrite1HInputPerMillion: entry.CacheWrite1HInputPerMillion,
					UnitPrice:                   entry.UnitPrice, Currency: entry.Currency}
				if entry.Provider != nil {
					if id, ok := providerIDs[strings.ToLower(*entry.Provider)]; ok {
						price.ProviderID = &id
					}
				}
				prices = append(prices, price)
			}
			if _, err = usage.CreateRevision(ctx, tx, p.UserID(), effective, prices, s.VendorKind); err != nil {
				return err
			}
			if _, err = runtime.Publish(ctx, tx, p.UserID()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Server) resolveSlots(ctx context.Context, tx pgx.Tx, providerID string, entry *ProviderEntry, existing *existingProvider, ok bool, bindings map[string]string) ([]resolvedSlot, error) {
	resolved := make([]resolvedSlot, 0, len(entry.Slots))
	for j := range entry.Slots {
		slot := &entry.Slots[j]
		slotID := access.NewID()
		var credentialID *string
		if ok {
			if current, present := existing.Slots[slot.Name]; present {
				slotID = current.ID
				credentialID = current.CredentialID
			}
		}
		if slot.CredentialRef != nil {
			if secret, supplied := bindings[*slot.CredentialRef]; supplied {
				same, err := s.bindingMatches(ctx, tx, credentialID, secret)
				if err != nil {
					return nil, err
				}
				if !same {
					stored, err := s.StoreCredential(ctx, tx, providerID, secret)
					if err != nil {
						return nil, err
					}
					credentialID = &stored
				}
			}
		}
		resolved = append(resolved, resolvedSlot{entry: slot, id: slotID, credentialID: credentialID})
	}
	return resolved, nil
}

func (s *Server) bindChangedCredentials(ctx context.Context, tx pgx.Tx, providerID string, entry *ProviderEntry, existing *existingProvider, bindings map[string]string) error {
	changed := false
	for j := range entry.Slots {
		slot := &entry.Slots[j]
		if slot.CredentialRef == nil {
			continue
		}
		secret, supplied := bindings[*slot.CredentialRef]
		if !supplied {
			continue
		}
		current, present := existing.Slots[slot.Name]
		if !present {
			continue
		}
		same, err := s.bindingMatches(ctx, tx, current.CredentialID, secret)
		if err != nil {
			return err
		}
		if same {
			continue
		}
		stored, err := s.StoreCredential(ctx, tx, providerID, secret)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "UPDATE olp_go.provider_slots SET credential_id=$3 WHERE provider_id=$1 AND id=$2", providerID, current.ID, stored); err != nil {
			return err
		}
		changed = true
	}
	if changed {
		if _, err := tx.Exec(ctx, "UPDATE olp_go.providers SET slots_etag=$2,draft_dirty=true,updated_at=now() WHERE id=$1", providerID, access.NewID()); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) replaceDraftContents(ctx context.Context, tx pgx.Tx, providerID string, entry *ProviderEntry, resolved []resolvedSlot) error {
	desired := make([]string, 0, len(entry.Models))
	for _, m := range entry.Models {
		desired = append(desired, m.UpstreamModel)
	}
	if _, err := tx.Exec(ctx, "UPDATE olp_go.provider_models SET enabled=false,capabilities='[]'::jsonb WHERE provider_id=$1 AND NOT (upstream_model = ANY($2::text[]))", providerID, desired); err != nil {
		return err
	}
	for _, m := range entry.Models {
		display := m.DisplayName
		if display == "" {
			display = m.UpstreamModel
		}
		type capability struct {
			Operation   string  `json:"operation"`
			Surface     string  `json:"surface"`
			Mode        string  `json:"mode"`
			Source      string  `json:"source"`
			CertifiedAt *string `json:"certified_at"`
		}
		declared := make([]capability, 0, len(m.Capabilities))
		for _, c := range m.Capabilities {
			declared = append(declared, capability{Operation: c.Operation, Surface: c.Surface, Mode: c.Mode, Source: "declared"})
		}
		capabilities, _ := json.Marshal(declared)
		if _, err := tx.Exec(ctx, `INSERT INTO olp_go.provider_models(id,provider_id,upstream_model,display_name,enabled,capabilities) VALUES($1,$2,$3,$4,$5,$6)
            ON CONFLICT(provider_id,upstream_model) DO UPDATE SET display_name=excluded.display_name,enabled=excluded.enabled,capabilities=excluded.capabilities`, access.NewID(), providerID, m.UpstreamModel, display, m.Enabled, capabilities); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, "DELETE FROM olp_go.provider_slots WHERE provider_id=$1", providerID); err != nil {
		return err
	}
	for _, slot := range resolved {
		restrictions, _ := json.Marshal(struct {
			AllowedAPIKeys []string `json:"allowed_api_keys"`
			AllowedModels  []string `json:"allowed_models"`
			AllowedRoutes  []string `json:"allowed_routes"`
		}{orEmpty(slot.entry.Restrictions.AllowedAPIKeys), orEmpty(slot.entry.Restrictions.AllowedModels), orEmpty(slot.entry.Restrictions.AllowedRoutes)})
		limits, _ := json.Marshal(slot.entry.Limits)
		if _, err := tx.Exec(ctx, "INSERT INTO olp_go.provider_slots(id,provider_id,is_default,position,name,enabled,priority,weight,credential_id,restrictions,limits) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)", slot.id, providerID, slot.entry.IsDefault, slot.entry.Position, slot.entry.Name, slot.entry.Enabled, slot.entry.Priority, slot.entry.Weight, slot.credentialID, restrictions, limits); err != nil {
			return err
		}
	}
	return nil
}
