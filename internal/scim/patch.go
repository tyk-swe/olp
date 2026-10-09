package scim

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	filter "github.com/scim2/filter-parser/v2"
)

// Select evaluates a bounded value filter against one array in a single query.
type Select func([]any, filter.Expression) ([]int, error)

func (s *SQLFilter) Expression(e filter.Expression, base string) (string, error) {
	return s.expression(e, base, 0)
}

func Patch(current, body Document, group bool, selectValues Select) (Document, error) {
	if err := Schema(body, PatchSchema); err != nil {
		return nil, err
	}
	operations, ok := body["operations"].([]any)
	if !ok || len(operations) == 0 || len(operations) > 32 {
		return nil, Invalid("Provide between one and 32 PATCH operations.")
	}
	result := Clone(current)
	for _, raw := range operations {
		op, ok := raw.(map[string]any)
		if !ok {
			return nil, Invalid("PATCH operations must be objects.")
		}
		kind := strings.ToLower(String(op, "op"))
		if kind != "add" && kind != "remove" && kind != "replace" {
			return nil, Invalid("Use add, remove or replace.")
		}
		path, ok := op["path"].(string)
		if op["path"] != nil && !ok {
			return nil, Fail(400, "invalidPath", "The PATCH path must be a string.")
		}
		value, provided := op["value"]
		if kind != "remove" && !provided {
			return nil, Invalid("This operation requires a value.")
		}
		if path == "" {
			if kind == "remove" {
				return nil, Fail(400, "noTarget", "Remove requires a path.")
			}
			fields, ok := value.(map[string]any)
			if !ok {
				return nil, Invalid("An operation without a path requires an object.")
			}
			for key, v := range fields {
				if err := change(result, []string{Canonical(key)}, kind, v, group); err != nil {
					return nil, err
				}
			}
			continue
		}
		if !boundedExpression(path) {
			return nil, Fail(400, "invalidPath", "The PATCH path is too complex.")
		}
		p, err := filter.ParsePathNumber([]byte(path))
		if err != nil {
			return nil, Fail(400, "invalidPath", "The PATCH path is invalid.")
		}
		parts := pathParts(p.AttributePath)
		if err = mutable(parts, group); err != nil {
			return nil, err
		}
		if p.ValueExpression == nil {
			if err = change(result, parts, kind, value, group); err != nil {
				return nil, err
			}
			continue
		}
		parent, key, err := parentField(result, parts, false)
		if err != nil {
			return nil, err
		}
		items, ok := parent[key].([]any)
		if !ok {
			return nil, Fail(400, "noTarget", "The filtered attribute has no values.")
		}
		indices, err := selectValues(items, p.ValueExpression)
		if err != nil {
			return nil, err
		}
		if len(indices) == 0 {
			return nil, Fail(400, "noTarget", "No value matches the PATCH filter.")
		}
		if kind != "remove" && parts[len(parts)-1] == "emails" && ((p.SubAttribute != nil && strings.EqualFold(*p.SubAttribute, "primary") && value == true) || (p.SubAttribute == nil && hasPrimary(value))) {
			clearPrimary(items)
		}
		selected := map[int]bool{}
		for _, i := range indices {
			selected[i] = true
		}
		out := make([]any, 0, len(items))
		for i, item := range items {
			if !selected[i] {
				out = append(out, item)
				continue
			}
			if p.SubAttribute != nil {
				object, ok := item.(map[string]any)
				if !ok {
					return nil, Fail(400, "invalidPath", "A subattribute requires a complex value.")
				}
				if err = change(object, []string{Canonical(*p.SubAttribute)}, kind, value, true); err != nil {
					return nil, err
				}
				out = append(out, object)
			} else if kind != "remove" {
				if kind == "add" {
					if object, ok := item.(map[string]any); ok {
						if fields, ok := value.(map[string]any); ok {
							for k, v := range fields {
								object[k] = v
							}
							out = append(out, object)
							continue
						}
					}
				}
				if values, ok := value.([]any); ok {
					out = append(out, values...)
				} else {
					out = append(out, value)
				}
			}
		}
		parent[key] = out
	}
	data, err := json.Marshal(result)
	if err != nil || len(data) > 4*MaxBody {
		return nil, Fail(413, "tooLarge", "The resulting resource exceeds its size limit.")
	}
	return result, nil
}

func mutable(parts []string, group bool) error {
	if len(parts) == 0 {
		return Fail(400, "invalidPath", "An attribute path is required.")
	}
	switch parts[0] {
	case "id", "meta", "password":
		return Fail(400, "mutability", "This attribute is not writable.")
	case "groups":
		if !group {
			return Fail(400, "mutability", "User groups are managed through Group membership.")
		}
	}
	return nil
}

func parentField(d map[string]any, parts []string, create bool) (map[string]any, string, error) {
	p := d
	for _, key := range parts[:len(parts)-1] {
		next, ok := p[key].(map[string]any)
		if !ok {
			if !create {
				return nil, "", Fail(400, "noTarget", "The attribute is not assigned.")
			}
			next = map[string]any{}
			p[key] = next
		}
		p = next
	}
	return p, parts[len(parts)-1], nil
}

func change(d map[string]any, parts []string, kind string, value any, group bool) error {
	if err := mutable(parts, group); err != nil {
		return err
	}
	if len(parts) > 1 {
		if values, ok := d[parts[0]].([]any); ok {
			changed := false
			for _, raw := range values {
				object, ok := raw.(map[string]any)
				if !ok {
					return Fail(400, "invalidPath", "A subattribute requires a complex value.")
				}
				err := change(object, parts[1:], kind, value, true)
				if problem, ok := err.(*Error); ok && problem.Type == "noTarget" {
					continue
				}
				if err != nil {
					return err
				}
				changed = true
			}
			if !changed {
				return Fail(400, "noTarget", "The attribute is not assigned.")
			}
			return nil
		}
	}
	parent, key, err := parentField(d, parts, kind != "remove")
	if err != nil {
		return err
	}
	if kind == "remove" {
		if _, ok := parent[key]; !ok {
			return Fail(400, "noTarget", "The attribute is not assigned.")
		}
		delete(parent, key)
		return nil
	}
	if kind == "add" {
		if old, ok := parent[key].([]any); ok {
			if key == "emails" && hasPrimary(value) {
				clearPrimary(old)
			}
			values, ok := value.([]any)
			if !ok {
				values = []any{value}
			}
			for _, v := range values {
				duplicate := false
				for _, existing := range old {
					if reflect.DeepEqual(existing, v) {
						duplicate = true
						break
					}
					a, aok := existing.(map[string]any)
					b, bok := v.(map[string]any)
					if aok && bok && a["value"] != nil && fmt.Sprint(a["value"]) == fmt.Sprint(b["value"]) {
						for key, value := range b {
							a[key] = value
						}
						duplicate = true
						break
					}
				}
				if !duplicate {
					old = append(old, v)
				}
			}
			parent[key] = old
			return nil
		}
		if old, ok := parent[key].(map[string]any); ok {
			if fields, ok := value.(map[string]any); ok {
				for k, v := range fields {
					old[k] = v
				}
				return nil
			}
		}
	}
	parent[key] = value
	return nil
}

func hasPrimary(value any) bool {
	if object, ok := value.(map[string]any); ok {
		return object["primary"] == true
	}
	if list, ok := value.([]any); ok {
		for _, v := range list {
			if hasPrimary(v) {
				return true
			}
		}
	}
	return false
}
func clearPrimary(values []any) {
	for _, v := range values {
		if object, ok := v.(map[string]any); ok {
			if _, exists := object["primary"]; exists {
				object["primary"] = false
			}
		}
	}
}
