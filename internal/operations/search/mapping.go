package search

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
)

// Mappings qualify Perplexity Search to each reviewed native target. The
// source contract's token, locale and freshness controls have no target
// equivalent and are refused rather than approximated.
func Mappings() []operations.Mapping {
	return []operations.Mapping{
		{Source: identity(Perplexity), Target: identity(Tavily), Lower: func(s oif.Request, v oif.View) (oif.Document, []oif.Provenance, error) { return lower(s, v, Tavily) }, Project: project, Evidence: "search-perplexity-tavily/1"},
		{Source: identity(Perplexity), Target: identity(Exa), Lower: func(s oif.Request, v oif.View) (oif.Document, []oif.Provenance, error) { return lower(s, v, Exa) }, Project: project, Evidence: "search-perplexity-exa/1"},
		{Source: identity(Perplexity), Target: identity(Parallel), Lower: func(s oif.Request, v oif.View) (oif.Document, []oif.Provenance, error) { return lower(s, v, Parallel) }, Project: project, Evidence: "search-perplexity-parallel/1"},
	}
}

func parseDocument(v any) (oif.Document, error) {
	return oif.ParseJSON(operations.Raw(v), oif.Limits{})
}

// rootDomain is a bare registered domain: no TLD-only, path or dotless part.
func rootDomain(entry string) bool {
	if strings.HasPrefix(entry, ".") || strings.ContainsAny(entry, "/ :\\@") {
		return false
	}
	for _, label := range strings.Split(entry, ".") {
		if label == "" {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c >= 'A' && c <= 'Z' || c == '_') {
				return false
			}
		}
	}
	return strings.Contains(entry, ".")
}

func lower(source oif.Request, view oif.View, target string) (oif.Document, []oif.Provenance, error) {
	r := view.(Request)
	root := source.Document().Root()
	for _, m := range root.Members() {
		switch m.Name {
		case "query", "max_results", "search_type", "search_domain_filter":
		default:
			return oif.Document{}, nil, invalid(m.Name)
		}
	}
	if !r.webOnly {
		return oif.Document{}, nil, invalid("search_type")
	}
	query := text(member(root, "query"))
	maxRaw := json.RawMessage(strconv.FormatInt(r.max, 10))
	var out map[string]any
	switch target {
	case Tavily:
		if len(r.domains) > 0 {
			return oif.Document{}, nil, invalid("search_domain_filter")
		}
		out = map[string]any{"query": query, "max_results": maxRaw}
	case Exa:
		out = map[string]any{"query": query, "numResults": maxRaw, "contents": map[string]any{"highlights": true}}
		if len(r.domains) > 0 {
			name := "includeDomains"
			if r.deny {
				name = "excludeDomains"
			}
			var list []any
			for _, d := range r.domains {
				if !rootDomain(d) {
					return oif.Document{}, nil, invalid("search_domain_filter")
				}
				list = append(list, d, "*."+d)
			}
			out[name] = list
		}
	case Parallel:
		settings := map[string]any{"max_results": maxRaw}
		if len(r.domains) > 0 {
			name := "include_domains"
			if r.deny {
				name = "exclude_domains"
			}
			var list []any
			for _, d := range r.domains {
				if !strings.Contains(d, ".") || strings.ContainsAny(d, " \\@") || strings.Contains(d, "://") {
					return oif.Document{}, nil, invalid("search_domain_filter")
				}
				list = append(list, d)
			}
			settings["source_policy"] = map[string]any{name: list}
		}
		out = map[string]any{"search_queries": []string{query}, "advanced_settings": settings}
	}
	doc, err := parseDocument(out)
	return doc, nil, err
}

// searchID is a fresh gateway-generated identifier; upstream result
// identities never cross the projection.
func searchID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "search_" + hex.EncodeToString(b[:])
}

func datePrefix(v oif.Value) any {
	if v.Kind() != oif.String {
		return nil
	}
	s := text(v)
	if len(s) >= 10 && s[4] == '-' && s[7] == '-' {
		d := s[:10]
		if n, err := strconv.Atoi(strings.ReplaceAll(d, "-", "")); err == nil && n > 0 {
			return d
		}
	}
	return nil
}

func project(source oif.Request, result oif.Result, view oif.View, route string) (oif.Document, error) {
	root := result.Source().Root()
	rows := member(root, "results")
	out := []any{}
	max := requestLimit(source)
	dialect := view.(Result).Source.Descriptor().Dialect.ID
	for i, row := range rows.Elements() {
		if int64(i) >= max {
			return oif.Document{}, operations.Violation("/results", "search_result_count")
		}
		var snippet string
		switch dialect {
		case Tavily:
			if v := member(row, "content"); v.Kind() == oif.String {
				snippet = text(v)
			}
		case Exa:
			snippet = joinTexts(member(row, "highlights"))
		case Parallel:
			snippet = joinTexts(member(row, "excerpts"))
		}
		var date any
		switch dialect {
		case Exa:
			date = datePrefix(member(row, "publishedDate"))
		case Parallel:
			date = datePrefix(member(row, "publish_date"))
		}
		title := ""
		if v := member(row, "title"); v.Kind() == oif.String {
			title = text(v)
		}
		out = append(out, map[string]any{"title": title, "url": text(member(row, "url")), "snippet": snippet, "date": date, "last_updated": nil})
	}
	return parseDocument(map[string]any{"id": searchID(), "results": out})
}

func joinTexts(v oif.Value) string {
	var parts []string
	for _, e := range v.Elements() {
		if e.Kind() == oif.String {
			parts = append(parts, text(e))
		}
	}
	return strings.Join(parts, "\n...\n")
}
