package scim

import (
	"strings"

	filter "github.com/scim2/filter-parser/v2"
)

func SortPath(raw, schema string) ([]string, error) {
	if !boundedExpression(raw) {
		return nil, Fail(400, "invalidPath", "Invalid attribute path.")
	}
	p, err := filter.ParsePath([]byte(raw))
	if err != nil || p.ValueExpression != nil || p.SubAttribute != nil {
		return nil, Fail(400, "invalidPath", "Use an attribute or subattribute path.")
	}
	if schema != "" && !matchesCoreSchema(p.AttributePath, schema) {
		return nil, Fail(400, "invalidPath", "The attribute belongs to a different resource schema.")
	}
	return pathParts(p.AttributePath), nil
}

type selection struct {
	whole    bool
	children map[string]*selection
}

type Projection struct {
	tree    *selection
	include bool
}

func ParseProjection(attributes, excluded, schema string) (Projection, error) {
	if attributes != "" && excluded != "" {
		return Projection{}, Invalid("Use attributes or excludedAttributes, not both.")
	}
	if attributes == "" && excluded == "" {
		return Projection{}, nil
	}
	if len(attributes)+len(excluded) > 4096 {
		return Projection{}, Invalid("Too many selected attributes.")
	}
	raw := excluded
	if attributes != "" {
		raw = attributes
	}
	tree := &selection{}
	for text := range strings.SplitSeq(raw, ",") {
		parts, err := SortPath(strings.TrimSpace(text), schema)
		if err != nil {
			return Projection{}, err
		}
		node := tree
		for _, part := range parts {
			if node.children == nil {
				node.children = map[string]*selection{}
			}
			if node.children[part] == nil {
				node.children[part] = &selection{}
			}
			node = node.children[part]
		}
		node.whole = true
	}
	return Projection{tree: tree, include: attributes != ""}, nil
}

func (p Projection) Apply(d Document) Document {
	if p.tree == nil {
		return d
	}
	out := projectObject(map[string]any(d), p.tree, p.include)
	for _, key := range []string{"id", "schemas", "meta"} {
		if value, ok := d[key]; ok {
			out[key] = value
		}
	}
	return Document(out)
}

func projectObject(v map[string]any, tree *selection, include bool) map[string]any {
	out := make(map[string]any, len(v))
	for key, val := range v {
		node := tree.children[key]
		if node == nil {
			if !include {
				out[key] = val
			}
			continue
		}
		if node.whole {
			if include {
				out[key] = val
			}
			continue
		}
		out[key] = projectValue(val, node, include)
	}
	return out
}

func projectValue(value any, tree *selection, include bool) any {
	switch v := value.(type) {
	case map[string]any:
		return projectObject(v, tree, include)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = projectValue(item, tree, include)
		}
		return out
	default:
		return value
	}
}
