package scim

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	filter "github.com/scim2/filter-parser/v2"
)

type Select func([]any, filter.Expression) ([]int, error)

func (s *SQLFilter) Expression(e filter.Expression, base string) (string, error) {
	return s.expression(e, base, 0, nil)
}

type patchOperation struct {
	kind  string
	path  string
	value any
}

func Patch(current, body Document, group bool, selectValues Select) (Document, error) {
	if err := Schema(body, PatchSchema); err != nil {
		return nil, err
	}
	operations, ok := body["operations"].([]any)
	if !ok || len(operations) == 0 || len(operations) > 32 {
		return nil, Invalid("Provide between one and 32 PATCH operations.")
	}
	schema := UserSchema
	if group {
		schema = GroupSchema
	}
	result := Clone(current)
	for _, raw := range operations {
		if err := applyPatchOperation(result, raw, schema, group, selectValues); err != nil {
			return nil, err
		}
	}
	data, err := json.Marshal(result)
	if err != nil || len(data) > 4*MaxBody {
		return nil, Fail(413, "tooLarge", "The resulting resource exceeds its size limit.")
	}
	return result, nil
}

func parsePatchOperation(raw any) (patchOperation, error) {
	op, ok := raw.(map[string]any)
	if !ok {
		return patchOperation{}, Invalid("PATCH operations must be objects.")
	}
	kind := strings.ToLower(String(op, "op"))
	if kind != "add" && kind != "remove" && kind != "replace" {
		return patchOperation{}, Invalid("Use add, remove or replace.")
	}
	path, ok := op["path"].(string)
	if op["path"] != nil && !ok {
		return patchOperation{}, Fail(400, "invalidPath", "The PATCH path must be a string.")
	}
	value, provided := op["value"]
	if kind != "remove" && !provided {
		return patchOperation{}, Invalid("This operation requires a value.")
	}
	return patchOperation{kind: kind, path: path, value: value}, nil
}

func applyPatchOperation(result Document, raw any, schema string, group bool, selectValues Select) error {
	op, err := parsePatchOperation(raw)
	if err != nil {
		return err
	}
	if op.path == "" {
		return applyRootPatchOperation(result, op, group)
	}
	parsed, parts, err := parsePatchTarget(op.path, schema, group)
	if err != nil {
		return err
	}
	if parsed.ValueExpression == nil {
		return change(result, parts, op.kind, op.value, group)
	}
	return patchFiltered(result, parts, parsed, op.kind, op.value, selectValues)
}

func applyRootPatchOperation(result Document, op patchOperation, group bool) error {
	if op.kind == "remove" {
		return Fail(400, "noTarget", "Remove requires a path.")
	}
	fields, ok := op.value.(map[string]any)
	if !ok {
		return Invalid("An operation without a path requires an object.")
	}
	for key, v := range fields {
		if err := change(result, []string{Canonical(key)}, op.kind, v, group); err != nil {
			return err
		}
	}
	return nil
}

func parsePatchTarget(path, schema string, group bool) (filter.Path, []string, error) {
	if !boundedExpression(path) {
		return filter.Path{}, nil, Fail(400, "invalidPath", "The PATCH path is too complex.")
	}
	p, err := filter.ParsePathNumber([]byte(path))
	if err != nil {
		return filter.Path{}, nil, Fail(400, "invalidPath", "The PATCH path is invalid.")
	}
	if !matchesCoreSchema(p.AttributePath, schema) {
		return filter.Path{}, nil, Fail(400, "invalidPath", "The PATCH path belongs to a different resource schema.")
	}
	if p.ValueExpression != nil && !matchesValueFilterSchema(p.ValueExpression, schema) {
		return filter.Path{}, nil, Fail(400, "invalidFilter", "The PATCH filter belongs to a different resource schema.")
	}
	parts := pathParts(p.AttributePath)
	if err = mutable(parts, group); err != nil {
		return filter.Path{}, nil, err
	}
	return p, parts, nil
}

func matchesValueFilterSchema(e filter.Expression, schema string) bool {
	switch x := e.(type) {
	case *filter.AttributeExpression:
		return matchesCoreSchema(x.AttributePath, schema)
	case *filter.LogicalExpression:
		return matchesValueFilterSchema(x.Left, schema) && matchesValueFilterSchema(x.Right, schema)
	case *filter.NotExpression:
		return matchesValueFilterSchema(x.Expression, schema)
	default:
		return false
	}
}

func mutable(parts []string, group bool) error {
	if len(parts) == 0 {
		return Fail(400, "invalidPath", "An attribute path is required.")
	}
	switch parts[0] {
	case "id", "meta", "password":
		return Fail(400, "mutability", "This attribute is not writable.")
	case "roles":
		if !group {
			return Fail(400, "mutability", "This attribute is not writable.")
		}
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
			return changeSliceSubattribute(values, parts[1:], kind, value)
		}
	}
	parent, key, err := parentField(d, parts, kind != "remove")
	if err != nil {
		return err
	}
	switch kind {
	case "remove":
		return removeField(parent, key)
	case "add":
		addField(parent, key, value)
		return nil
	default:
		parent[key] = value
		return nil
	}
}

func changeSliceSubattribute(values []any, subParts []string, kind string, value any) error {
	changed := false
	for _, raw := range values {
		object, ok := raw.(map[string]any)
		if !ok {
			return Fail(400, "invalidPath", "A subattribute requires a complex value.")
		}
		err := change(object, subParts, kind, value, true)
		if problem, ok := errors.AsType[*Error](err); ok && problem.Type == "noTarget" {
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

func removeField(parent map[string]any, key string) error {
	if _, ok := parent[key]; !ok {
		return Fail(400, "noTarget", "The attribute is not assigned.")
	}
	delete(parent, key)
	return nil
}

func addField(parent map[string]any, key string, value any) {
	if old, ok := parent[key].([]any); ok {
		parent[key] = mergeSliceAdd(old, key, value)
		return
	}
	if old, ok := parent[key].(map[string]any); ok {
		if fields, ok := value.(map[string]any); ok {
			maps.Copy(old, fields)
			return
		}
	}
	if email, ok := value.(map[string]any); key == "emails" && ok {
		parent[key] = []any{email}
		return
	}
	parent[key] = value
}

func mergeSliceAdd(old []any, key string, value any) []any {
	out := slices.Clone(old)
	if key == "emails" && hasPrimary(value) {
		clearPrimary(out)
	}
	values, ok := value.([]any)
	if !ok {
		values = []any{value}
	}
	for _, candidate := range values {
		if !mergeSliceEntry(out, candidate) {
			out = append(out, candidate)
		}
	}
	return out
}

func mergeSliceEntry(entries []any, candidate any) bool {
	for _, existing := range entries {
		if reflect.DeepEqual(existing, candidate) {
			return true
		}
		a, aok := existing.(map[string]any)
		b, bok := candidate.(map[string]any)
		if aok && bok && a["value"] != nil && fmt.Sprint(a["value"]) == fmt.Sprint(b["value"]) {
			maps.Copy(a, b)
			return true
		}
	}
	return false
}

func hasPrimary(value any) bool {
	if object, ok := value.(map[string]any); ok {
		return object["primary"] == true
	}
	if list, ok := value.([]any); ok {
		return slices.ContainsFunc(list, hasPrimary)
	}
	return false
}

func clearPrimary(values []any) {
	for _, v := range values {
		object, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if _, exists := object["primary"]; exists {
			object["primary"] = false
		}
	}
}

func shouldClearFilteredPrimary(kind string, parts []string, p filter.Path, value any) bool {
	if kind == "remove" || parts[len(parts)-1] != "emails" {
		return false
	}
	if p.SubAttribute != nil {
		return strings.EqualFold(*p.SubAttribute, "primary") && value == true
	}
	return hasPrimary(value)
}

func patchFiltered(result Document, parts []string, p filter.Path, kind string, value any, selectValues Select) error {
	parent, key, err := parentField(result, parts, false)
	if err != nil {
		return err
	}
	items, ok := parent[key].([]any)
	if !ok {
		return Fail(400, "noTarget", "The filtered attribute has no values.")
	}
	indices, err := selectValues(items, p.ValueExpression)
	if err != nil {
		return err
	}
	if len(indices) == 0 {
		return Fail(400, "noTarget", "No value matches the PATCH filter.")
	}
	if shouldClearFilteredPrimary(kind, parts, p, value) {
		clearPrimary(items)
	}
	selected := make(map[int]bool, len(indices))
	for _, idx := range indices {
		selected[idx] = true
	}
	out := make([]any, 0, len(items))
	for i, item := range items {
		if !selected[i] {
			out = append(out, item)
			continue
		}
		replacements, err := patchFilteredItem(item, p.SubAttribute, kind, value)
		if err != nil {
			return err
		}
		out = append(out, replacements...)
	}
	parent[key] = out
	return nil
}

func patchFilteredItem(item any, subAttribute *string, kind string, value any) ([]any, error) {
	if subAttribute != nil {
		object, ok := item.(map[string]any)
		if !ok {
			return nil, Fail(400, "invalidPath", "A subattribute requires a complex value.")
		}
		if err := change(object, []string{Canonical(*subAttribute)}, kind, value, true); err != nil {
			return nil, err
		}
		return []any{object}, nil
	}
	if kind == "remove" {
		return nil, nil
	}
	if kind == "add" {
		if object, ok := item.(map[string]any); ok {
			if fields, ok := value.(map[string]any); ok {
				maps.Copy(object, fields)
				return []any{object}, nil
			}
		}
	}
	if values, ok := value.([]any); ok {
		return values, nil
	}
	return []any{value}, nil
}
