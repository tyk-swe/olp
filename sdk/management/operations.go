// Package management provides contract-generated operations and a bounded HTTP
// client for OLP's management API. The server remains the authority for scopes,
// project access, validation, ETags and mutation audit.
package management

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed operations.gen.json
var registry []byte

// Parameter describes a declared path, query or conditional header parameter.
type Parameter struct {
	Name     string `json:"name"`
	In       string `json:"in"`
	Required bool   `json:"required"`
}

// Operation is generated from openapi/management.json by make api.
type Operation struct {
	Name         string          `json:"name"`
	Group        string          `json:"group"`
	Method       string          `json:"method"`
	Path         string          `json:"path"`
	Description  string          `json:"description"`
	Scopes       [][]string      `json:"scopes"`
	Parameters   []Parameter     `json:"parameters"`
	BodyRequired bool            `json:"body_required"`
	InputSchema  json.RawMessage `json:"input_schema"`
}

// Operations returns an independent registry so callers cannot change other
// clients' contract metadata.
func Operations() []Operation {
	var operations []Operation
	if err := json.Unmarshal(registry, &operations); err != nil {
		panic(fmt.Sprintf("generated management registry: %v", err))
	}
	return operations
}

// Lookup finds a management-token operation by its contract operationId.
func Lookup(name string) (Operation, bool) {
	for _, operation := range Operations() {
		if operation.Name == name {
			return operation, true
		}
	}
	return Operation{}, false
}
