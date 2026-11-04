package promotion

import (
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/attribution"
	"github.com/tyk-swe/olp/internal/billing"
	"slices"
	"strings"
)

type PlanItem struct {
	Kind   string `json:"kind"`
	Key    string `json:"key"`
	Action string `json:"action"`
	Detail string `json:"detail"`
}

type PlanResult struct {
	Digest    string     `json:"digest"`
	Actions   []PlanItem `json:"actions"`
	Conflicts []PlanItem `json:"conflicts"`
	Blockers  []PlanItem `json:"blockers"`
}

func (r *PlanResult) Add(kind, key, action, detail string) {
	r.Actions = append(r.Actions, PlanItem{kind, key, action, detail})
}

func (r *PlanResult) Conflict(kind, key, detail string) {
	r.Conflicts = append(r.Conflicts, PlanItem{kind, key, "conflict", detail})
}

func (r *PlanResult) Blocker(kind, key, detail string) {
	r.Blockers = append(r.Blockers, PlanItem{kind, key, "blocker", detail})
}

func (r *PlanResult) Sort() {
	by := func(a, b PlanItem) int {
		if c := strings.Compare(a.Kind, b.Kind); c != 0 {
			return c
		}
		if c := strings.Compare(a.Key, b.Key); c != 0 {
			return c
		}
		if c := strings.Compare(a.Action, b.Action); c != 0 {
			return c
		}
		return strings.Compare(a.Detail, b.Detail)
	}
	slices.SortFunc(r.Actions, by)
	slices.SortFunc(r.Conflicts, by)
	slices.SortFunc(r.Blockers, by)
}

type ExistingSlot struct {
	ID           string
	CredentialID *string
	// PluginDigest is the plugin build whose grant enrollment created the
	// credential version, or "" for a pasted one.
	PluginDigest string
	ProfileID    string
}

type ExistingProvider struct {
	NetworkCredentialID *string
	BillingImport       *billing.ImportState
	ID                  string
	Name                string
	Kind                string
	State               string
	ProjectID           *string
	Slots               map[string]ExistingSlot
}

type ExistingRoute struct {
	ID        string
	ProjectID *string
	State     string
}

type ExistingDraft struct {
	ID        string
	ProjectID *string
}

type StateView struct {
	ProjectAttributionBudgets  map[string]access.AttributionBudgets
	ProjectBudgets             map[string]*access.BudgetPolicy
	ProjectTemplates           map[string]access.LimitTemplates
	ProjectRouteGroups         map[string]access.RouteGroups
	Projects                   map[string]string
	ProjectEndUserPolicies     map[string]*access.EndUserPolicy
	ProjectAttributionPolicies map[string]*attribution.Policy
	ProjectName                map[string]string
	ProjectOrganization        map[string]*string
	Providers                  map[string]*ExistingProvider
	Routes                     map[string]*ExistingRoute
	Drafts                     map[string]*ExistingDraft
}

func (v *StateView) ProjectOf(id *string) *string {
	if id == nil {
		return nil
	}
	if name, ok := v.ProjectName[*id]; ok {
		return &name
	}
	return nil
}

func NormalizeDocument(doc *Document) {
	for i := range doc.Projects {
		doc.Projects[i].Name = strings.TrimSpace(doc.Projects[i].Name)
		doc.Projects[i].Budget = access.NormalizedBudget(doc.Projects[i].Budget)
		for _, members := range doc.Projects[i].RouteGroups {
			slices.Sort(members)
		}
	}
	for i := range doc.Providers {
		p := &doc.Providers[i]
		p.Name = strings.TrimSpace(p.Name)
		if p.Project != nil {
			v := strings.TrimSpace(*p.Project)
			p.Project = &v
		}
		for j := range p.Models {
			m := &p.Models[j]
			m.UpstreamModel = strings.TrimSpace(m.UpstreamModel)
			m.DisplayName = strings.TrimSpace(m.DisplayName)
		}

		for j := range p.Slots {
			slot := &p.Slots[j]
			slot.Name = strings.TrimSpace(slot.Name)
			if slot.CredentialRef != nil {
				v := strings.TrimSpace(*slot.CredentialRef)
				slot.CredentialRef = &v
			}
		}
	}
	for i := range doc.Routes {
		rt := &doc.Routes[i]
		rt.Slug = strings.TrimSpace(rt.Slug)
		if rt.Project != nil {
			v := strings.TrimSpace(*rt.Project)
			rt.Project = &v
		}
		for j := range rt.Targets {
			t := &rt.Targets[j]
			t.Provider = strings.TrimSpace(t.Provider)
			t.ProviderModel = strings.TrimSpace(t.ProviderModel)
		}
	}
	if doc.Pricing != nil {
		for i := range doc.Pricing.Prices {
			e := &doc.Pricing.Prices[i]
			if e.Provider != nil {
				v := strings.TrimSpace(*e.Provider)
				e.Provider = &v
			}
		}
	}
	for i := range doc.Discounts {
		d := &doc.Discounts[i]
		d.ProviderKind = strings.TrimSpace(d.ProviderKind)
		d.Multiplier = strings.TrimSpace(d.Multiplier)
		if d.VendorID != nil {
			v := strings.TrimSpace(*d.VendorID)
			d.VendorID = &v
		}
		if d.Provider != nil {
			v := strings.TrimSpace(*d.Provider)
			d.Provider = &v
		}
	}
	for i := range doc.RateCards {
		c := &doc.RateCards[i]
		c.Owner.Kind = strings.TrimSpace(c.Owner.Kind)
		c.Markup = strings.TrimSpace(c.Markup)
		if c.Owner.Organization != nil {
			v := strings.TrimSpace(*c.Owner.Organization)
			c.Owner.Organization = &v
		}
		if c.Owner.Project != nil {
			v := strings.TrimSpace(*c.Owner.Project)
			c.Owner.Project = &v
		}
		for j := range c.Prices {
			p := &c.Prices[j]
			p.Model = strings.TrimSpace(p.Model)
			p.Currency = strings.ToUpper(strings.TrimSpace(p.Currency))
			if p.VendorID != nil {
				v := strings.TrimSpace(*p.VendorID)
				p.VendorID = &v
			}
			if p.Provider != nil {
				v := strings.TrimSpace(*p.Provider)
				p.Provider = &v
			}
		}
	}
}
