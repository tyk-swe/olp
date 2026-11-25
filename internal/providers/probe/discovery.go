package probe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/connectors/providerconfig"
	"github.com/tyk-swe/olp/internal/providers/connection"
)

func (p *Prober) ListModels(ctx context.Context, t Target) ([]string, error) {
	models, err := p.ListModelFacts(ctx, t)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, m := range models {
		ids = append(ids, m.Name)
	}
	return ids, nil
}

type DiscoveredModel struct {
	Name, Display string
	HasDisplay    bool
	Metadata      map[string]json.RawMessage
}

type Listing struct {
	Models   []DiscoveredModel
	Complete bool
	Detail   string
}

func (p *Prober) ListCompleteModelFacts(ctx context.Context, t Target) (Listing, error) {
	return p.listModelFacts(ctx, t, true)
}

func (p *Prober) ListModelFacts(ctx context.Context, t Target) ([]DiscoveredModel, error) {
	listing, err := p.listModelFacts(ctx, t, false)
	return listing.Models, err
}

func (p *Prober) listModelFacts(ctx context.Context, t Target, strict bool) (Listing, error) {
	listing, discovery, err := t.Connection.Transport().ModelListing()
	if err != nil {
		return Listing{}, err
	}
	if !discovery {
		return p.probeDeclaredModels(ctx, t)
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	out := []DiscoveredModel{}
	seen, cursors := map[string]bool{}, map[string]bool{}
	path := listing.Path
	for {
		status, body, err := p.call(ctx, t, http.MethodGet, path, nil)
		if err != nil {
			return Listing{}, err
		}
		if status != http.StatusOK {
			return Listing{}, statusError(t, status, body)
		}
		var page map[string]json.RawMessage
		if json.Unmarshal(body, &page) != nil {
			return Listing{}, &Failure{Reason: ProviderProtocolError, Detail: "Invalid model discovery response."}
		}
		var items []map[string]json.RawMessage
		if json.Unmarshal(page[listing.Models], &items) != nil || items == nil {
			return Listing{}, &Failure{Reason: ProviderProtocolError, Detail: "Discovery response is missing its model array."}
		}
		for _, item := range items {
			var id, display string
			if strict {
				rawID, hasID := item[listing.ID]
				if !hasID || json.Unmarshal(rawID, &id) != nil {
					return Listing{}, &Failure{Reason: ProviderProtocolError, Detail: "Discovery response holds a model without a valid identifier."}
				}
				if rawDisplay, hasDisplay := item[listing.Display]; hasDisplay && json.Unmarshal(rawDisplay, &display) != nil {
					return Listing{}, &Failure{Reason: ProviderProtocolError, Detail: "Discovery response holds a model without a valid display name."}
				}
			} else {
				_ = json.Unmarshal(item[listing.ID], &id)
				_ = json.Unmarshal(item[listing.Display], &display)
			}
			name := strings.TrimPrefix(id, listing.TrimIDPrefix)
			if strict && connection.ValidModelName("model", name) != nil {
				return Listing{}, &Failure{Reason: ProviderProtocolError, Detail: "Discovery response holds a model without a valid identifier."}
			}
			if connection.ValidModelName("model", name) != nil || seen[name] {
				continue
			}
			seen[name] = true
			hasDisplay := display != ""
			if display == "" {
				display = name
			}
			facts := map[string]json.RawMessage{}
			for from, to := range map[string]string{"inputTokenLimit": "context_length", "outputTokenLimit": "max_output_tokens", "inputModalities": "input_modalities", "outputModalities": "output_modalities"} {
				if v, ok := item[from]; ok {
					facts[to] = v
				}
			}
			if len(facts) > 0 {
				facts["source"], _ = json.Marshal(providerconfig.Value(t.Connection.Document().Endpoint))
				facts["observed_at"], _ = json.Marshal(time.Now().UTC())
			}
			out = append(out, DiscoveredModel{Name: name, Display: display, HasDisplay: hasDisplay, Metadata: facts})
			if len(out) == 2000 && !strict {
				return Listing{Models: out}, nil
			}
			if len(out) > 2000 {
				return Listing{Models: out[:2000], Detail: "The model listing exceeds the 2,000 model bound, so completeness cannot be proved."}, nil
			}
		}
		if strict {
			cursor, final, incomplete, err := strictNextCursor(listing, page)
			if err != nil {
				return Listing{}, err
			}
			if final {
				return Listing{Models: out, Complete: incomplete == "", Detail: incomplete}, nil
			}
			if len(cursor) > 2048 || cursors[cursor] {
				return Listing{}, &Failure{Reason: ProviderProtocolError, Detail: "Invalid repeated discovery continuation."}
			}
			cursors[cursor] = true
			path = listing.Path + "?" + url.Values{listing.Parameter: []string{cursor}}.Encode()
			continue
		}
		cursor, err := nextCursor(listing, page)
		if err != nil || cursor == "" {
			return Listing{Models: out}, err
		}
		if len(cursor) > 2048 || cursors[cursor] {
			return Listing{}, &Failure{Reason: ProviderProtocolError, Detail: "Invalid repeated discovery continuation."}
		}
		cursors[cursor] = true
		path = listing.Path + "?" + url.Values{listing.Parameter: []string{cursor}}.Encode()
	}
}

func (p *Prober) probeDeclaredModels(ctx context.Context, t Target) (Listing, error) {
	ctx, cancel := Budget(ctx)
	defer cancel()
	names := append([]string{}, t.ProbeModels...)
	if t.Connection.Document().Kind == connectors.KindAzureOpenAI {
		names = append(names, providerconfig.Value(t.Connection.Document().Deployment))
	}
	for name := range t.Connection.Document().Options.Models {
		names = append(names, name)
	}
	for name := range t.Connection.Document().Options.Bindings {
		names = append(names, name)
	}
	slices.Sort(names)
	if len(names) == 0 {
		return Listing{}, &Failure{Reason: ModelRequired, Detail: "Declare a model before probing this vendor."}
	}
	out := []DiscoveredModel{}
	previous := ""
	for _, name := range names {
		if name == previous {
			continue
		}
		previous = name
		tuple := DefaultTuple(t.Connection.Transport())
		err := p.Certify(ctx, t, name, tuple)
		if err != nil && t.Connection.Document().Kind == connectors.KindAzureOpenAI {
			tuple.Operation = "embeddings"
			err = p.Certify(ctx, t, name, tuple)
		}
		if err != nil {
			return Listing{}, err
		}
		out = append(out, DiscoveredModel{Name: name, Display: name})
	}
	return Listing{Models: out, Detail: "This provider has no model listing that can prove completeness."}, nil
}

func nextCursor(l connectors.ModelListing, page map[string]json.RawMessage) (string, error) {
	if l.Cursor == "" {
		return "", nil
	}
	if l.More != "" {
		var more bool
		_ = json.Unmarshal(page[l.More], &more)
		if !more {
			return "", nil
		}
	}
	var cursor string
	_ = json.Unmarshal(page[l.Cursor], &cursor)
	if cursor == "" && l.More != "" {
		return "", &Failure{Reason: ProviderProtocolError, Detail: "Discovery continuation is missing its cursor."}
	}
	return cursor, nil
}

func strictNextCursor(l connectors.ModelListing, page map[string]json.RawMessage) (cursor string, final bool, incomplete string, err error) {
	if l.More == "" || l.Cursor == "" || l.Parameter == "" {
		return "", true, "The model listing does not state whether another page follows, so completeness cannot be proved.", nil
	}
	rawMore, ok := page[l.More]
	if !ok {
		return "", true, "The model listing is missing its pagination field, so completeness cannot be proved.", nil
	}
	var decoded any
	if json.Unmarshal(rawMore, &decoded) != nil {
		return "", true, "The model listing holds a malformed pagination field, so completeness cannot be proved.", nil
	}
	more, isBool := decoded.(bool)
	if !isBool {
		return "", true, "The model listing holds a malformed pagination field, so completeness cannot be proved.", nil
	}
	if !more {
		return "", true, "", nil
	}
	rawCursor, ok := page[l.Cursor]
	if !ok || json.Unmarshal(rawCursor, &cursor) != nil || cursor == "" {
		return "", false, "", &Failure{Reason: ProviderProtocolError, Detail: "Discovery continuation is missing its cursor."}
	}
	return cursor, false, "", nil
}
