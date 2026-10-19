// Package search owns the frozen Perplexity Search contract and the reviewed
// native search dialects its qualified mappings serve: Tavily, Exa and
// Parallel. A search result is evidence-complete without usage counters.
package search

import (
	"slices"
	"strconv"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
)

const (
	Perplexity = "perplexity-search"
	Tavily     = "tavily-search"
	Exa        = "exa-search"
	Parallel   = "parallel-search"
)

// maxRows is the largest page of results the frozen contract admits.
const maxRows = 20

func identity(id string) oif.Identity        { return oif.Identity{ID: id, Revision: operations.Revision} }
func member(v oif.Value, k string) oif.Value { return operations.Member(v, k) }
func text(v oif.Value) string                { return operations.String(v) }
func invalid(p string) error {
	return operations.Invalid(p, "The field is outside the frozen native search contract.")
}
func conflict(p string) error {
	return operations.Error("policy_conflict", p, "output_policy_coverage", "The output policy cannot inspect this native search result.")
}

var dateFilters = []string{"search_after_date_filter", "search_before_date_filter", "last_updated_after_filter", "last_updated_before_filter", "search_recency_filter"}

// domains is the request's allow-or-deny domain filter, exactly one mode.
func domains(root oif.Value) (entries []string, deny bool, err error) {
	filter := member(root, "search_domain_filter")
	if operations.Optional(filter) {
		return nil, false, nil
	}
	if filter.Kind() != oif.Array || len(filter.Elements()) == 0 || len(filter.Elements()) > maxRows {
		return nil, false, invalid("search_domain_filter")
	}
	for i, e := range filter.Elements() {
		if e.Kind() != oif.String {
			return nil, false, invalid("search_domain_filter")
		}
		entry := text(e)
		denied := len(entry) > 0 && entry[0] == '-'
		if denied {
			entry = entry[1:]
		}
		if i == 0 {
			deny = denied
		} else if denied != deny {
			return nil, false, invalid("search_domain_filter")
		}
		if entry == "" {
			return nil, false, invalid("search_domain_filter")
		}
		entries = append(entries, entry)
	}
	return entries, deny, nil
}

type Request struct {
	source  oif.Request
	max     int64
	domains []string
	deny    bool
	webOnly bool
}

func (r Request) Schema() oif.Identity { return identity("search") }

type Result struct{ Source oif.Result }

func (r Result) Schema() oif.Identity { return identity("search") }

func Definitions() []operations.Dialect {
	out := []operations.Dialect{}
	for _, id := range []string{Perplexity, Tavily, Exa, Parallel} {
		d := operations.Dialect{Identity: identity(id), Operation: identity("search"), Surface: "openai", Label: id + " web search", Address: operations.Address{RelativePath: "search"}, Evidence: id + "-results/1",
			Request:    func(source oif.Request) (oif.View, error) { return parseRequest(source, id) },
			Result:     func(source oif.Request, result oif.Result) (oif.View, error) { return parseResult(source, result, id) },
			InputText:  func(request oif.Request) ([]operations.Text, error) { return inputText(request, id) },
			OutputText: func(result oif.Result) ([]operations.Text, error) { return outputText(result, id) }}
		d.Probe = probe(id)
		if id != Perplexity {
			d.Surface = "native"
		}
		switch id {
		case Perplexity:
			d.Documentation = "https://docs.perplexity.ai/api-reference/search-post"
			d.RequestSchema = operations.ObjectSchema(map[string]any{"query": map[string]any{"type": "string"}, "max_results": map[string]any{"type": "integer", "minimum": 1, "maximum": maxRows}}, "query")
			d.ResultSchema = operations.ObjectSchema(map[string]any{"id": map[string]any{"type": "string"}, "results": map[string]any{"type": "array"}}, "id", "results")
		case Tavily:
			d.Documentation = "https://docs.tavily.com/api-reference/endpoint/search"
			d.RequestSchema = operations.ObjectSchema(map[string]any{"query": map[string]any{"type": "string"}, "max_results": map[string]any{"type": "integer"}}, "query", "max_results")
			d.ResultSchema = operations.ObjectSchema(map[string]any{"results": map[string]any{"type": "array"}}, "results")
		case Exa:
			d.Documentation = "https://docs.exa.ai/reference/search"
			d.RequestSchema = operations.ObjectSchema(map[string]any{"query": map[string]any{"type": "string"}, "numResults": map[string]any{"type": "integer"}, "contents": map[string]any{"type": "object"}}, "query", "numResults", "contents")
			d.ResultSchema = operations.ObjectSchema(map[string]any{"results": map[string]any{"type": "array"}}, "results")
		case Parallel:
			d.Documentation = "https://docs.parallel.ai/api-reference/search"
			d.RequestSchema = operations.ObjectSchema(map[string]any{"search_queries": map[string]any{"type": "array"}, "advanced_settings": map[string]any{"type": "object"}}, "search_queries", "advanced_settings")
			d.ResultSchema = operations.ObjectSchema(map[string]any{"results": map[string]any{"type": "array"}}, "results")
		}
		out = append(out, d)
	}
	return out
}

func probe(id string) func(string) []byte {
	return func(model string) []byte {
		switch id {
		case Tavily:
			return operations.Raw(map[string]any{"query": "OLP certification probe", "max_results": 1})
		case Exa:
			return operations.Raw(map[string]any{"query": "OLP certification probe", "numResults": 1, "contents": map[string]any{"highlights": true}})
		case Parallel:
			return operations.Raw(map[string]any{"search_queries": []string{"OLP certification probe"}, "advanced_settings": map[string]any{"max_results": 1}})
		}
		return operations.Raw(map[string]any{"query": "OLP certification probe", "max_results": 1})
	}
}

func parseRequest(source oif.Request, id string) (oif.View, error) {
	r := Request{source: source, webOnly: true}
	root := source.Document().Root()
	if root.Kind() != oif.Object {
		return r, invalid("request")
	}
	switch id {
	case Perplexity:
		return perplexityRequest(r, root)
	case Tavily:
		for _, m := range root.Members() {
			if m.Name != "query" && m.Name != "max_results" {
				return r, invalid(m.Name)
			}
		}
		if err := boundedQuery(root, "max_results"); err != nil {
			return r, err
		}
	case Exa:
		for _, m := range root.Members() {
			switch m.Name {
			case "query", "numResults", "contents", "includeDomains", "excludeDomains":
			default:
				return r, invalid(m.Name)
			}
		}
		if err := boundedQuery(root, "numResults"); err != nil {
			return r, err
		}
		c := member(root, "contents")
		if c.Kind() != oif.Object {
			return r, invalid("contents")
		}
		for _, m := range c.Members() {
			if m.Name != "highlights" || m.Value.Raw() != "true" {
				return r, invalid("contents." + m.Name)
			}
		}
		for _, name := range []string{"includeDomains", "excludeDomains"} {
			if v := member(root, name); !operations.Optional(v) {
				if v.Kind() != oif.Array || len(v.Elements()) > 2*maxRows {
					return r, invalid(name)
				}
				for _, e := range v.Elements() {
					if e.Kind() != oif.String {
						return r, invalid(name)
					}
				}
			}
		}
	case Parallel:
		for _, m := range root.Members() {
			if m.Name != "search_queries" && m.Name != "advanced_settings" {
				return r, invalid(m.Name)
			}
		}
		queries := member(root, "search_queries")
		if queries.Kind() != oif.Array || len(queries.Elements()) == 0 || len(queries.Elements()) > maxRows {
			return r, invalid("search_queries")
		}
		for _, e := range queries.Elements() {
			if e.Kind() != oif.String {
				return r, invalid("search_queries")
			}
		}
		settings := member(root, "advanced_settings")
		if settings.Kind() != oif.Object {
			return r, invalid("advanced_settings")
		}
		for _, m := range settings.Members() {
			switch m.Name {
			case "max_results":
				if n, ok := operations.Int(m.Value); !ok || n < 1 || n > maxRows {
					return r, invalid("advanced_settings.max_results")
				}
			case "source_policy":
				if m.Value.Kind() != oif.Object {
					return r, invalid("advanced_settings.source_policy")
				}
				for _, p := range m.Value.Members() {
					switch p.Name {
					case "include_domains", "exclude_domains":
						if p.Value.Kind() != oif.Array || len(p.Value.Elements()) > maxRows {
							return r, invalid("advanced_settings.source_policy." + p.Name)
						}
						for _, e := range p.Value.Elements() {
							if e.Kind() != oif.String {
								return r, invalid("advanced_settings.source_policy." + p.Name)
							}
						}
					default:
						return r, invalid("advanced_settings.source_policy." + p.Name)
					}
				}
			default:
				return r, invalid("advanced_settings." + m.Name)
			}
		}
		if member(settings, "max_results").Kind() != oif.Number {
			return r, invalid("advanced_settings.max_results")
		}
	}
	return r, nil
}

// boundedQuery validates the shared query plus maximum-result fields.
func boundedQuery(root oif.Value, maximum string) error {
	if member(root, "query").Kind() != oif.String {
		return invalid("query")
	}
	if n, ok := operations.Int(member(root, maximum)); !ok || n < 1 || n > maxRows {
		return invalid(maximum)
	}
	return nil
}

func perplexityRequest(r Request, root oif.Value) (oif.View, error) {
	for _, m := range root.Members() {
		switch m.Name {
		case "query", "max_results", "search_type", "max_tokens", "max_tokens_per_page", "country", "search_language_filter", "search_domain_filter":
		default:
			if !slices.Contains(dateFilters, m.Name) {
				return r, invalid(m.Name)
			}
		}
	}
	if member(root, "query").Kind() != oif.String {
		return r, invalid("query")
	}
	r.max = 10
	if v := member(root, "max_results"); !operations.Optional(v) {
		n, ok := operations.Int(v)
		if !ok || n < 1 || n > maxRows {
			return r, invalid("max_results")
		}
		r.max = n
	}
	if v := member(root, "search_type"); !operations.Optional(v) {
		if v.Kind() != oif.String {
			return r, invalid("search_type")
		}
		r.webOnly = text(v) == "web"
	}
	for _, name := range []string{"max_tokens", "max_tokens_per_page"} {
		if v := member(root, name); !operations.Optional(v) {
			if n, ok := operations.Int(v); !ok || n < 1 || n > 1000000 {
				return r, invalid(name)
			}
		}
	}
	if v := member(root, "country"); !operations.Optional(v) && (v.Kind() != oif.String || len(text(v)) != 2) {
		return r, invalid("country")
	}
	if v := member(root, "search_language_filter"); !operations.Optional(v) {
		if v.Kind() != oif.Array || len(v.Elements()) > maxRows {
			return r, invalid("search_language_filter")
		}
		for _, e := range v.Elements() {
			if e.Kind() != oif.String || len(text(e)) != 2 {
				return r, invalid("search_language_filter")
			}
		}
	}
	for _, name := range dateFilters {
		if v := member(root, name); !operations.Optional(v) && v.Kind() != oif.String {
			return r, invalid(name)
		}
	}
	entries, deny, err := domains(root)
	if err != nil {
		return r, err
	}
	r.domains, r.deny = entries, deny
	return r, nil
}

func parseResult(request oif.Request, source oif.Result, id string) (oif.View, error) {
	if _, err := parseRequest(request, id); err != nil {
		return nil, err
	}
	r := Result{Source: source}
	root := source.Source().Root()
	fail := func(field, requirement string) (oif.View, error) {
		return r, operations.Violation(field, requirement)
	}
	results := member(root, "results")
	if results.Kind() != oif.Array {
		return fail("/results", "search_results")
	}
	if id == Perplexity {
		if member(root, "id").Kind() != oif.String {
			return fail("/id", "search_id")
		}
		want := requestLimit(request)
		if int64(len(results.Elements())) > want {
			return fail("/results", "search_result_count")
		}
		for i, row := range results.Elements() {
			path := "/results/" + strconv.Itoa(i)
			for _, name := range []string{"title", "url", "snippet"} {
				if member(row, name).Kind() != oif.String {
					return fail(path+"/"+name, "search_result_fields")
				}
			}
		}
		return r, nil
	}
	for i, row := range results.Elements() {
		if member(row, "url").Kind() != oif.String {
			return fail("/results/"+strconv.Itoa(i)+"/url", "search_result_url")
		}
	}
	return r, nil
}

// requestLimit is the validated per-request result ceiling (default 10).
func requestLimit(request oif.Request) int64 {
	if v, ok := operations.Int(member(request.Document().Root(), "max_results")); ok && v >= 1 && v <= maxRows {
		return v
	}
	return 10
}

func inputText(request oif.Request, id string) ([]operations.Text, error) {
	if _, err := parseRequest(request, id); err != nil {
		return nil, err
	}
	root := request.Document().Root()
	if id == Parallel {
		out := []operations.Text{}
		for i, q := range member(root, "search_queries").Elements() {
			out = append(out, operations.Text{Pointer: "/search_queries/" + strconv.Itoa(i), Value: text(q)})
		}
		return out, nil
	}
	return []operations.Text{{Pointer: "/query", Value: text(member(root, "query"))}}, nil
}

// outputText covers each dialect's result text: title, url, snippet and the
// content or excerpt fields. Reviewed metadata fields pass without inspection;
// anything the contract does not name fails closed unless it carries no text.
func outputText(result oif.Result, id string) ([]operations.Text, error) {
	out := []operations.Text{}
	known := map[string]bool{"id": true, "results": true, "server_time": true}
	switch id {
	case Tavily:
		known["query"], known["response_time"], known["request_id"] = true, true, true
	case Exa:
		known["requestId"], known["costDollars"], known["searchTime"] = true, true, true
	case Parallel:
		known["search_id"], known["session_id"], known["usage"], known["warnings"] = true, true, true, true
	}
	root := result.Source().Root()
	for _, f := range root.Members() {
		if !known[f.Name] && !inert(f.Value) {
			return nil, conflict(operations.Pointer("", f.Name))
		}
	}
	inspect := map[string]bool{"title": true, "url": true, "snippet": true, "content": true, "author": true, "text": true, "summary": true}
	metadata := map[string]bool{"id": true, "score": true, "date": true, "last_updated": true, "publishedDate": true, "publish_date": true, "favicon": true, "image": true, "highlightScores": true}
	for i, row := range member(root, "results").Elements() {
		path := "/results/" + strconv.Itoa(i)
		for _, f := range row.Members() {
			p := operations.Pointer(path, f.Name)
			switch {
			case inspect[f.Name]:
				if f.Value.Kind() == oif.String {
					out = append(out, operations.Text{Pointer: p, Value: text(f.Value)})
				} else if !inert(f.Value) {
					return nil, conflict(p)
				}
			case f.Name == "highlights" || f.Name == "excerpts":
				if f.Value.Kind() != oif.Array && !inert(f.Value) {
					return nil, conflict(p)
				}
				for j, e := range f.Value.Elements() {
					if e.Kind() == oif.String {
						out = append(out, operations.Text{Pointer: p + "/" + strconv.Itoa(j), Value: text(e)})
					} else if !inert(e) {
						return nil, conflict(p + "/" + strconv.Itoa(j))
					}
				}
			case metadata[f.Name]:
			default:
				if !inert(f.Value) {
					return nil, conflict(p)
				}
			}
		}
	}
	return out, nil
}

// inert reports whether a field carries no text the output policy could miss:
// null, scalar and empty-container values only.
func inert(v oif.Value) bool {
	switch v.Kind() {
	case oif.Absent, oif.Null, oif.Number, oif.Boolean:
		return true
	case oif.Array:
		return len(v.Elements()) == 0
	case oif.Object:
		return len(v.Members()) == 0
	}
	return false
}
