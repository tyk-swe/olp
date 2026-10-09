package scim

import (
	"strings"

	filter "github.com/scim2/filter-parser/v2"
)

func SortPath(raw string) ([]string, error) {
	if !boundedExpression(raw) {
		return nil, Fail(400, "invalidPath", "Invalid attribute path.")
	}
	p, e := filter.ParsePath([]byte(raw))
	if e != nil || p.ValueExpression != nil || p.SubAttribute != nil {
		return nil, Fail(400, "invalidPath", "Use an attribute or subattribute path.")
	}
	return pathParts(p.AttributePath), nil
}

type selection struct {
	whole    bool
	children map[string]*selection
}

func Project(d Document, attributes, excluded string) (Document, error) {
	if attributes != "" && excluded != "" {
		return nil, Invalid("Use attributes or excludedAttributes, not both.")
	}
	if attributes == "" && excluded == "" {
		return d, nil
	}
	if len(attributes)+len(excluded) > 4096 {
		return nil, Invalid("Too many selected attributes.")
	}
	raw := excluded
	if attributes != "" {
		raw = attributes
	}
	tree := &selection{}
	for _, text := range strings.Split(raw, ",") {
		parts, err := SortPath(strings.TrimSpace(text))
		if err != nil {
			return nil, err
		}
		node := tree
		for _, p := range parts {
			if node.children == nil {
				node.children = map[string]*selection{}
			}
			if node.children[p] == nil {
				node.children[p] = &selection{}
			}
			node = node.children[p]
		}
		node.whole = true
	}
	out := projectValue(map[string]any(d), tree, attributes != "").(map[string]any)
	for _, key := range []string{"id", "schemas", "meta"} {
		if value, ok := d[key]; ok {
			out[key] = value
		}
	}
	return Document(out), nil
}

func projectValue(value any, tree *selection, include bool) any {
	switch v := value.(type) {
	case map[string]any:
		out := map[string]any{}
		for key, value := range v {
			node := tree.children[key]
			if node == nil {
				if !include {
					out[key] = value
				}
				continue
			}
			if node.whole {
				if include {
					out[key] = value
				}
				continue
			}
			out[key] = projectValue(value, node, include)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, value := range v {
			out[i] = projectValue(value, tree, include)
		}
		return out
	default:
		return value
	}
}
