package code

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/apikeys"
	"github.com/tyk-swe/olp/internal/codemode/codeadmin"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
)

const namespace = "openllmproxy.dev/config/v2"

type destination struct {
	id       string
	existing bool
}

type resolved[T any] struct {
	ref      string
	to       destination
	identity string
	input    T
}

func derivedID(kind, project, ref string) (string, error) {
	name, err := json.Marshal([]string{namespace, "code", kind, project, ref})
	if err != nil {
		return "", err
	}
	return uuid.NewSHA1(uuid.NameSpaceURL, name).String(), nil
}

func codeRef(kind, id string) string { return "code/" + kind + "/" + id }

func optionalRef(kind string, id *string) *string {
	if id == nil {
		return nil
	}
	return new(codeRef(kind, *id))
}

func lookup(absent string, ids []string) (destination, error) {
	switch len(ids) {
	case 0:
		return destination{id: absent}, nil
	case 1:
		return destination{id: ids[0], existing: true}, nil
	}
	return destination{}, ambiguous()
}

func mapped(ref, project, derived string, unique bool, candidates []codeadmin.Candidate) (destination, error) {
	if direct := slices.IndexFunc(candidates, func(c codeadmin.Candidate) bool { return c.ID == ref && c.ProjectID == project }); direct >= 0 {
		if !candidates[direct].ScopeMatches {
			return destination{}, httpkit.Invalid("code", "A destination artifact reference cannot change its immutable resource scope.")
		}
		return destination{id: ref, existing: true}, nil
	}
	if stable := slices.IndexFunc(candidates, func(c codeadmin.Candidate) bool { return c.ID == derived }); stable >= 0 {
		if candidates[stable].ProjectID != project || !candidates[stable].ScopeMatches {
			return destination{}, httpkit.Invalid("code", "A mapped destination resource cannot change its immutable scope.")
		}
		return destination{id: derived, existing: true}, nil
	}
	natural := func(c codeadmin.Candidate) bool { return c.ProjectID == project && c.NaturalMatches }
	first := slices.IndexFunc(candidates, natural)
	if first < 0 {
		return destination{id: derived}, nil
	}
	if unique && !slices.ContainsFunc(candidates[first+1:], natural) {
		return destination{id: candidates[first].ID, existing: true}, nil
	}
	return destination{}, ambiguous()
}

func ambiguous() error {
	return httpkit.Invalid("code", "The destination has ambiguous code resource identities.")
}

func distinct[T any](items []resolved[T]) error {
	for i, item := range items {
		if slices.ContainsFunc(items[:i], func(other resolved[T]) bool {
			return other.to.id == item.to.id || item.identity != "" && other.identity == item.identity
		}) {
			return httpkit.Invalid("code", "Multiple artifact resources resolve to the same destination identity.")
		}
	}
	return nil
}

func destinations[T any](items []resolved[T]) map[string]string {
	return promotion.Associate(items, func(item resolved[T]) (string, string) { return item.ref, item.to.id })
}

func projectOf(directory promotion.Directory, name string) (string, error) {
	id, ok := directory.ID(name)
	if !ok {
		return "", httpkit.Invalid("code.project", "Create the destination project and enroll its credentials before promoting code configuration.")
	}
	return id, nil
}

func bound(ref string, values map[string]string) (string, error) {
	value, ok := values[ref]
	if !ok {
		return "", httpkit.Invalid("code_bindings", "Bind every referenced destination credential, owner and API key explicitly.")
	}
	return value, nil
}

func boundOr(ref *string, values map[string]string) string {
	if ref == nil {
		return ""
	}
	return values[*ref]
}

func owner(ctx context.Context, q database.Queryer, ref *string, project string, users map[string]string) (*string, error) {
	if ref == nil {
		return nil, nil
	}
	id, err := bound(*ref, users)
	if err != nil {
		return nil, err
	}
	member, err := access.ActiveProjectMember(ctx, q, project, id)
	if err != nil {
		return nil, err
	}
	if !member {
		return nil, httpkit.Invalid("code_bindings.users", "The personal pool owner must be an active destination project member.")
	}
	return &id, nil
}

func key(ctx context.Context, q database.Queryer, ref, project string, owner *string, keys map[string]string) (string, error) {
	id, err := bound(ref, keys)
	if err != nil {
		return "", err
	}
	allowed, err := apikeys.ProjectKeyUsable(ctx, q, id, project, owner)
	if err != nil {
		return "", err
	}
	if !allowed {
		return "", httpkit.Invalid("code_bindings.api_keys", "The destination key must belong to the project and personal pool owner.")
	}
	return id, nil
}

func keysOf(ctx context.Context, q database.Queryer, refs []string, project string, owner *string, keys map[string]string) ([]string, error) {
	return promotion.TryMapIndex(refs, func(_ int, ref string) (string, error) { return key(ctx, q, ref, project, owner, keys) })
}

func compared[T any](ctx context.Context, q database.Queryer, r resolved[T], read func(context.Context, database.Queryer, string) (T, error)) (contract.ConfigurationPlanItemAction, error) {
	if !r.to.existing {
		return contract.ConfigurationPlanItemActionCreate, nil
	}
	current, err := read(ctx, q, r.to.id)
	if err != nil {
		return "", err
	}
	equal, err := same(current, r.input)
	return replacement(!equal), err
}

func same[T any](left, right T) (bool, error) {
	a, err := json.Marshal(left)
	if err != nil {
		return false, err
	}
	b, err := json.Marshal(right)
	return string(a) == string(b), err
}

func replacement(changed bool) contract.ConfigurationPlanItemAction {
	if changed {
		return contract.ConfigurationPlanItemActionReplace
	}
	return contract.ConfigurationPlanItemActionNoop
}
