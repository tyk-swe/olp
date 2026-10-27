// Package search owns the frozen Perplexity Search contract and the reviewed
// native search dialects its qualified mappings serve: Tavily, Exa, Parallel
// and Brave. A search result is evidence-complete without usage counters.
package search

import (
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
)

const (
	Perplexity = "perplexity-search"
	Tavily     = "tavily-search"
	Exa        = "exa-search"
	Parallel   = "parallel-search"
	Brave      = "brave-search"
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
	return []operations.Dialect{
		searchDialect(Perplexity, "openai", "https://docs.perplexity.ai/api-reference/search-post", "search", operations.TransportJSON,
			operations.ObjectSchema(map[string]any{"query": map[string]any{"type": "string"}, "max_results": map[string]any{"type": "integer", "minimum": 1, "maximum": maxRows}}, "query"),
			operations.ObjectSchema(map[string]any{"id": map[string]any{"type": "string"}, "results": map[string]any{"type": "array"}}, "id", "results")),
		searchDialect(Tavily, "native", "https://docs.tavily.com/api-reference/endpoint/search", "search", operations.TransportJSON,
			operations.ObjectSchema(map[string]any{"query": map[string]any{"type": "string"}, "max_results": map[string]any{"type": "integer"}}, "query", "max_results"),
			operations.ObjectSchema(map[string]any{"results": map[string]any{"type": "array"}}, "results")),
		searchDialect(Exa, "native", "https://docs.exa.ai/reference/search", "search", operations.TransportJSON,
			operations.ObjectSchema(map[string]any{"query": map[string]any{"type": "string"}, "numResults": map[string]any{"type": "integer"}, "contents": map[string]any{"type": "object"}}, "query", "numResults", "contents"),
			operations.ObjectSchema(map[string]any{"results": map[string]any{"type": "array"}}, "results")),
		searchDialect(Parallel, "native", "https://docs.parallel.ai/api-reference/search", "search", operations.TransportJSON,
			operations.ObjectSchema(map[string]any{"search_queries": map[string]any{"type": "array"}, "advanced_settings": map[string]any{"type": "object"}}, "search_queries", "advanced_settings"),
			operations.ObjectSchema(map[string]any{"results": map[string]any{"type": "array"}}, "results")),
		{
			Identity: identity(Brave), Operation: identity("search"), Surface: "native", Label: Brave + " web search", Documentation: "https://api-dashboard.search.brave.com/api-reference/web/search/get", Address: operations.Address{RelativePath: "res/v1/web/search"}, Evidence: Brave + "-results/1",
			Transport: operations.TransportGetQuery,
			Query:     braveQuery,
			Request:   func(source oif.Request) (oif.View, error) { return parseRequest(source, Brave) },
			Result: func(source oif.Request, result oif.Result) (oif.View, error) {
				return parseResult(source, result, Brave)
			},
			InputText:     func(request oif.Request) ([]operations.Text, error) { return inputText(request, Brave) },
			OutputText:    func(result oif.Result) ([]operations.Text, error) { return outputText(result, Brave) },
			Probe:         probe(Brave),
			RequestSchema: operations.ObjectSchema(map[string]any{"q": map[string]any{"type": "string"}, "count": map[string]any{"type": "integer", "minimum": 1, "maximum": maxRows}}, "q", "count"),
			ResultSchema:  operations.ObjectSchema(map[string]any{"web": map[string]any{"type": "object"}}, "web"),
		},
	}
}

func searchDialect(id, surface, documentation, relativePath string, transport operations.Transport, requestSchema, resultSchema []byte) operations.Dialect {
	return operations.Dialect{
		Identity: identity(id), Operation: identity("search"), Surface: surface, Label: id + " web search", Documentation: documentation, Address: operations.Address{RelativePath: relativePath}, Evidence: id + "-results/1",
		Transport:     transport,
		Request:       func(source oif.Request) (oif.View, error) { return parseRequest(source, id) },
		Result:        func(source oif.Request, result oif.Result) (oif.View, error) { return parseResult(source, result, id) },
		InputText:     func(request oif.Request) ([]operations.Text, error) { return inputText(request, id) },
		OutputText:    func(result oif.Result) ([]operations.Text, error) { return outputText(result, id) },
		Probe:         probe(id),
		RequestSchema: requestSchema,
		ResultSchema:  resultSchema,
	}
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
		case Brave:
			return operations.Raw(map[string]any{"q": "OLP certification probe", "count": 1, "result_filter": "web"})
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
	case Brave:
		if err := braveRequest(root); err != nil {
			return r, err
		}
	}
	return r, nil
}

const maxBraveQueryChars = 600

func braveRequest(root oif.Value) error {
	for _, m := range root.Members() {
		switch m.Name {
		case "q", "count", "offset", "freshness", "result_filter":
		default:
			return invalid(m.Name)
		}
	}
	q := member(root, "q")
	if q.Kind() != oif.String || text(q) == "" || utf8.RuneCountInString(text(q)) > maxBraveQueryChars {
		return invalid("q")
	}
	if n, ok := operations.Int(member(root, "count")); !ok || n < 1 || n > maxRows {
		return invalid("count")
	}
	if v := member(root, "offset"); !operations.Optional(v) {
		if n, ok := operations.Int(v); !ok || n < 0 || n > 9 {
			return invalid("offset")
		}
	}
	if v := member(root, "freshness"); !operations.Optional(v) {
		if v.Kind() != oif.String || !validFreshness(text(v)) {
			return invalid("freshness")
		}
	}
	if v := member(root, "result_filter"); !operations.Optional(v) {
		if v.Kind() != oif.String || text(v) != "web" {
			return invalid("result_filter")
		}
	}
	return nil
}

func validFreshness(value string) bool {
	switch value {
	case "pd", "pw", "pm", "py":
		return true
	}
	start, end, ok := strings.Cut(value, "to")
	if !ok {
		return false
	}
	from, errFrom := time.Parse(time.DateOnly, start)
	to, errTo := time.Parse(time.DateOnly, end)
	return errFrom == nil && errTo == nil && !from.After(to)
}

func braveQuery(request oif.Request) (url.Values, error) {
	if _, err := parseRequest(request, Brave); err != nil {
		return nil, err
	}
	root := request.Document().Root()
	values := url.Values{}
	values.Set("q", text(member(root, "q")))
	count, _ := operations.Int(member(root, "count"))
	values.Set("count", strconv.FormatInt(count, 10))
	if v := member(root, "offset"); !operations.Optional(v) {
		offset, _ := operations.Int(v)
		values.Set("offset", strconv.FormatInt(offset, 10))
	}
	if v := member(root, "freshness"); !operations.Optional(v) {
		values.Set("freshness", text(v))
	}
	values.Set("result_filter", "web")
	values.Set("text_decorations", "false")
	return values, nil
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
	if id == Brave {
		rows := member(member(root, "web"), "results")
		if rows.Kind() != oif.Array {
			return fail("/web/results", "search_results")
		}
		count, ok := operations.Int(member(request.Document().Root(), "count"))
		if !ok || int64(len(rows.Elements())) > count {
			return fail("/web/results", "search_result_count")
		}
		for i, row := range rows.Elements() {
			if member(row, "url").Kind() != oif.String {
				return fail("/web/results/"+strconv.Itoa(i)+"/url", "search_result_url")
			}
		}
		return r, nil
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
	if id == Brave {
		return []operations.Text{{Pointer: "/q", Value: text(member(root, "q"))}}, nil
	}
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
	if id == Brave {
		return braveOutputText(result)
	}
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

func braveOutputText(result oif.Result) ([]operations.Text, error) {
	out := []operations.Text{}
	root := result.Source().Root()
	for _, f := range root.Members() {
		switch f.Name {
		case "type":
		case "query":
			collectStrings(f.Value, operations.Pointer("", f.Name), &out)
		case "web":
			if err := braveWebText(f.Value, &out); err != nil {
				return nil, err
			}
		default:
			if !inert(f.Value) {
				return nil, conflict(operations.Pointer("", f.Name))
			}
		}
	}
	return out, nil
}

func braveWebText(web oif.Value, out *[]operations.Text) error {
	for _, f := range web.Members() {
		switch f.Name {
		case "type", "family_friendly":
		case "results":
			for i, row := range f.Value.Elements() {
				if err := braveRowText(row, "/web/results/"+strconv.Itoa(i), out); err != nil {
					return err
				}
			}
		default:
			if !inert(f.Value) {
				return conflict(operations.Pointer("/web", f.Name))
			}
		}
	}
	return nil
}

func braveRowText(row oif.Value, path string, out *[]operations.Text) error {
	inspect := map[string]bool{"title": true, "url": true, "description": true}
	containers := map[string]bool{"profile": true, "meta_url": true, "thumbnail": true}
	metadata := map[string]bool{"is_source_local": true, "is_source_both": true, "page_age": true, "page_fetched": true, "fetched_content_timestamp": true, "language": true, "family_friendly": true, "type": true, "subtype": true, "is_live": true, "age": true}
	for _, f := range row.Members() {
		p := operations.Pointer(path, f.Name)
		switch {
		case inspect[f.Name]:
			if f.Value.Kind() == oif.String {
				*out = append(*out, operations.Text{Pointer: p, Value: text(f.Value)})
			} else if !inert(f.Value) {
				return conflict(p)
			}
		case f.Name == "extra_snippets":
			if f.Value.Kind() != oif.Array && !inert(f.Value) {
				return conflict(p)
			}
			for j, e := range f.Value.Elements() {
				if e.Kind() == oif.String {
					*out = append(*out, operations.Text{Pointer: p + "/" + strconv.Itoa(j), Value: text(e)})
				} else if !inert(e) {
					return conflict(p + "/" + strconv.Itoa(j))
				}
			}
		case containers[f.Name]:
			collectStrings(f.Value, p, out)
		case metadata[f.Name]:
		default:
			if !inert(f.Value) {
				return conflict(p)
			}
		}
	}
	return nil
}

func collectStrings(v oif.Value, pointer string, out *[]operations.Text) {
	switch v.Kind() {
	case oif.String:
		*out = append(*out, operations.Text{Pointer: pointer, Value: text(v)})
	case oif.Array:
		for i, e := range v.Elements() {
			collectStrings(e, pointer+"/"+strconv.Itoa(i), out)
		}
	case oif.Object:
		for _, m := range v.Members() {
			collectStrings(m.Value, operations.Pointer(pointer, m.Name), out)
		}
	}
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
