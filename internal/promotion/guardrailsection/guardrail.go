package guardrail

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
)

const (
	invalidGuardrails    = "Use unique project/name pairs, builtin.regex, and bounded content policies."
	invalidConfiguration = "Invalid guardrail configuration."
	unboundCredential    = "Bind every new guardrail credential to a sealed JSON credential."
)

type Guardrail struct {
	Project       string
	Name          string
	Type          string
	Configuration json.RawMessage
	TimeoutMS     int64
	OnError       string
	credentials   []reference
}

type reference struct {
	ref     promotion.Ref
	network bool
}

func builtin(kind string) bool { return strings.HasPrefix(kind, "builtin.") }

func label(value string) bool {
	return utf8.ValidString(value) && utf8.RuneCountInString(value) >= 1 && utf8.RuneCountInString(value) <= 100 && !strings.ContainsFunc(value, unicode.IsControl)
}

func parseGuardrails(entries []contract.ConfigurationGuardrail) ([]Guardrail, error) {
	if len(entries) > 1000 {
		return nil, access.Invalid("guardrails", "Declare at most 1000 guardrails.")
	}
	parsed, err := collect(entries, parseGuardrail)
	if err != nil {
		return nil, err
	}
	if duplicated(parsed, func(g Guardrail) portable { return keyOf(g.Project, g.Name) }) {
		return nil, access.Invalid("guardrails", invalidGuardrails)
	}
	return parsed, nil
}

func parseGuardrail(entry contract.ConfigurationGuardrail) (Guardrail, error) {
	project, name := strings.TrimSpace(entry.Project), strings.TrimSpace(entry.Name)
	if !label(name) || (project != "" && !label(project)) || entry.Configuration == nil {
		return Guardrail{}, access.Invalid("guardrails", invalidGuardrails)
	}
	configuration, err := json.Marshal(*entry.Configuration)
	if err != nil {
		return Guardrail{}, access.Invalid("guardrails", invalidConfiguration)
	}
	parsed, err := withCredentials(Guardrail{
		Project:       project,
		Name:          name,
		Type:          string(entry.Type),
		Configuration: configuration,
		TimeoutMS:     cmp.Or(dereference(entry.TimeoutMs), 1000),
		OnError:       cmp.Or(string(dereference(entry.OnError)), guardrails.FailClosed),
	})
	if err != nil || guardrails.ValidateDefinition(guardrails.Revision{Type: parsed.Type, Configuration: configuration, TimeoutMS: parsed.TimeoutMS, OnError: parsed.OnError}) != nil {
		return Guardrail{}, access.Invalid("guardrails", invalidConfiguration)
	}
	return parsed, nil
}

func withCredentials(g Guardrail) (Guardrail, error) {
	if builtin(g.Type) {
		return g, nil
	}
	var c guardrails.ExternalConfig
	if err := json.Unmarshal(g.Configuration, &c); err != nil {
		return Guardrail{}, err
	}
	configuration, err := json.Marshal(c)
	if err != nil {
		return Guardrail{}, err
	}
	references := kept([]reference{{promotion.Ref(c.CredentialRef), false}, {promotion.Ref(networkCredential(c)), true}}, func(r reference) bool { return r.ref != "" })
	return Guardrail{Project: g.Project, Name: g.Name, Type: g.Type, Configuration: configuration, TimeoutMS: g.TimeoutMS, OnError: g.OnError, credentials: references}, nil
}

func networkCredential(c guardrails.ExternalConfig) string {
	if c.Network == nil {
		return ""
	}
	return c.Network.CredentialID
}

func renderGuardrail(g Guardrail) (contract.ConfigurationGuardrail, error) {
	var configuration map[string]any
	if err := json.Unmarshal(g.Configuration, &configuration); err != nil {
		return contract.ConfigurationGuardrail{}, err
	}
	timeout, onError := g.TimeoutMS, contract.ConfigurationGuardrailOnError(g.OnError)
	return contract.ConfigurationGuardrail{Configuration: &configuration, Name: g.Name, OnError: &onError, Project: g.Project, TimeoutMs: &timeout, Type: contract.ConfigurationGuardrailType(g.Type)}, nil
}

func claims(definitions []Guardrail) []promotion.Claim {
	return slices.Concat(mapped(definitions, func(g Guardrail) []promotion.Claim { return mapped(g.credentials, claim) })...)
}

func claim(r reference) promotion.Claim {
	return promotion.Claim{Ref: r.ref, Accept: promotion.SealedOnly("Guardrail credentials require sealed secret bindings.", func(secret string) error {
		field := "secret_bindings." + string(r.ref)
		if len(secret) > promotion.MaxSecret || guardrails.ValidateCredential([]byte(secret)) != nil {
			return access.Invalid(field, "Supply a bounded guardrail credential object.")
		}
		if r.network && egress.ValidateConnectionSecret([]byte(secret)) != nil {
			return access.Invalid(field, "Supply a valid network credential object.")
		}
		return nil
	})}
}

func planGuardrails(ctx context.Context, q access.Queryer, desired []Guardrail, names promotion.Names) ([]promotion.Item, error) {
	if len(desired) == 0 {
		return nil, nil
	}
	directory, err := project.ReadDirectory(ctx, q)
	if err != nil {
		return nil, err
	}
	planned, err := collect(desired, func(g Guardrail) ([]promotion.Item, error) {
		plugin, err := planPlugin(ctx, q, g)
		if err != nil {
			return nil, err
		}
		definition, err := planDefinition(ctx, q, directory, names, g)
		return append(plugin, definition), err
	})
	return slices.Concat(planned...), err
}

type plugin struct {
	name                       string
	approved, confined, usable bool
}

func planPlugin(ctx context.Context, q access.Queryer, g Guardrail) ([]promotion.Item, error) {
	if g.Type != string(contract.ConfigurationGuardrailTypePlugin) {
		return nil, nil
	}
	var c guardrails.ExternalConfig
	if err := json.Unmarshal(g.Configuration, &c); err != nil {
		return nil, err
	}
	installed, err := optional(ctx, q, func(row pgx.Row) (plugin, error) {
		var p plugin
		err := row.Scan(&p.name, &p.approved, &p.confined, &p.usable)
		return p, err
	}, "SELECT manifest->>'name',approved_at IS NOT NULL,module IS NOT NULL,COALESCE((manifest->>'guardrail')::boolean,false) FROM olp.plugins WHERE digest=$1", c.PluginVersion)
	switch {
	case err != nil:
		return nil, err
	case installed == nil:
		return []promotion.Item{promotion.Blocker(contract.ConfigurationPlanItemKindPlugin, c.PluginVersion, "plugin_not_installed")}, nil
	case !installed.approved:
		return []promotion.Item{promotion.Blocker(contract.ConfigurationPlanItemKindPlugin, c.PluginVersion, "plugin_not_approved")}, nil
	case !installed.confined || !installed.usable || installed.name != c.PluginID:
		return []promotion.Item{promotion.Conflict(contract.ConfigurationPlanItemKindPlugin, c.PluginVersion, "A matching confined guardrail plugin is required.")}, nil
	}
	return nil, nil
}

func planDefinition(ctx context.Context, q access.Queryer, directory project.Directory, names promotion.Names, g Guardrail) (promotion.Item, error) {
	key := g.Project + "/" + g.Name
	id, present := owner(directory, g.Project)
	if !present && !names.Project(g.Project) {
		return promotion.Conflict(contract.ConfigurationPlanItemKindGuardrail, key, "The owning project is not declared or present at the destination."), nil
	}
	if !present {
		return item(contract.ConfigurationPlanItemKindGuardrail, key, contract.ConfigurationPlanItemActionCreate, "Publish a reusable bounded content-policy definition."), nil
	}
	current, err := findActive(ctx, q, id, g.Name)
	if err != nil {
		return promotion.Item{}, err
	}
	const detail = "Published routes retain their copied policies; definition revisions remain local."
	if current == nil {
		return item(contract.ConfigurationPlanItemKindGuardrail, key, contract.ConfigurationPlanItemActionCreate, detail), nil
	}
	if current.Type != g.Type {
		return promotion.Conflict(contract.ConfigurationPlanItemKindGuardrail, key, "A definition type is immutable; use a new name."), nil
	}
	configuration, err := rewriteCredentials(current.Type, current.Configuration, credentialName(ctx, q))
	if err != nil {
		return promotion.Item{}, err
	}
	if current.Name == g.Name && equal(Guardrail{Configuration: configuration, TimeoutMS: current.TimeoutMS, OnError: current.OnError}, g) {
		return item(contract.ConfigurationPlanItemKindGuardrail, key, contract.ConfigurationPlanItemActionReuse, detail), nil
	}
	return item(contract.ConfigurationPlanItemKindGuardrail, key, contract.ConfigurationPlanItemActionReplace, detail), nil
}

func GuardrailApplier(desired Section) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, p access.Principal) error {
		return each(desired.Guardrails, func(g Guardrail) error { return applyGuardrail(ctx, tx, p, g) })
	}
}

func applyGuardrail(ctx context.Context, tx pgx.Tx, p access.Principal, g Guardrail) error {
	project, err := projectID(ctx, tx, g.Project)
	if err != nil {
		return err
	}
	if err := p.Project(project, access.Change); err != nil {
		return err
	}
	configuration, err := rewriteCredentials(g.Type, g.Configuration, credentialID(ctx, tx, project))
	if err != nil {
		return err
	}
	current, err := findActive(ctx, tx, installationOr(project), g.Name)
	if err != nil {
		return err
	}
	local := Guardrail{Configuration: configuration, TimeoutMS: g.TimeoutMS, OnError: g.OnError}
	if current != nil && current.Name == g.Name && current.Type == g.Type && equal(Guardrail{Configuration: current.Configuration, TimeoutMS: current.TimeoutMS, OnError: current.OnError}, local) {
		if current.ActiveRevisionID != nil && *current.ActiveRevisionID == current.RevisionID {
			return nil
		}
		return guardrails.ActivateRevision(ctx, tx, p.UserID(), current.ID, current.RevisionID)
	}
	saved := &guardrails.Definition{ProjectID: installationOr(project), Name: g.Name, Type: g.Type, Configuration: configuration, TimeoutMS: g.TimeoutMS, OnError: g.OnError}
	if err := guardrails.Save(ctx, tx, p.UserID(), current, saved); err != nil {
		return err
	}
	return guardrails.ActivateRevision(ctx, tx, p.UserID(), saved.ID, saved.RevisionID)
}

func owner(directory project.Directory, name string) (string, bool) {
	if name == "" {
		return "installation", true
	}
	return directory.ID(name)
}

func item(kind contract.ConfigurationPlanItemKind, key string, action contract.ConfigurationPlanItemAction, detail string) promotion.Item {
	return promotion.Item{Kind: kind, Key: key, Action: action, Detail: detail}
}

func findActive(ctx context.Context, q access.Queryer, projectID, name string) (*guardrails.Definition, error) {
	current, err := guardrails.FindActive(ctx, q, projectID, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return current, err
}

func equal(a, b Guardrail) bool {
	x, xok := canonical(a.Configuration)
	y, yok := canonical(b.Configuration)
	return xok && yok && bytes.Equal(x, y) && a.TimeoutMS == b.TimeoutMS && a.OnError == b.OnError
}

func canonical(raw json.RawMessage) ([]byte, bool) {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil, false
	}
	encoded, err := json.Marshal(value)
	return encoded, err == nil
}

func dereference[T any](pointer *T) T {
	if pointer == nil {
		var zero T
		return zero
	}
	return *pointer
}
