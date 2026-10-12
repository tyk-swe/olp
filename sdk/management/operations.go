// Package management provides contract-generated operations and a bounded HTTP
// client for OLP's management API. The server remains the authority for scopes,
// project access, validation, ETags and mutation audit.
package management

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
)

//go:embed operations.gen.json
var registry []byte

// Parameter describes a declared path, query or conditional header parameter.
type Parameter struct {
	Name     string `json:"name"`
	In       string `json:"in"`
	Required bool   `json:"required"`
	Type     string `json:"type"`
}

// Operation is generated from openapi/management.json by make api.
type Operation struct {
	Name            string            `json:"name"`
	Group           string            `json:"group"`
	Method          string            `json:"method"`
	ReadOnly        bool              `json:"read_only"`
	Path            string            `json:"path"`
	Description     string            `json:"description"`
	Scopes          [][]string        `json:"scopes"`
	Parameters      []Parameter       `json:"parameters"`
	BodyRequired    bool              `json:"body_required"`
	InputSchema     json.RawMessage   `json:"input_schema"`
	ResponseHeaders map[string]string `json:"response_headers"`
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

// ResponseMetadata returns only the safe response facts declared by this
// operation's contract. Parent ETags describe the committed atomic transition,
// including replay, rather than a later read of mutable parent state.
func (operation Operation) ResponseMetadata(header http.Header) map[string]string {
	metadata := map[string]string{}
	for name, field := range operation.ResponseHeaders {
		if value := header.Get(name); value != "" {
			metadata[field] = value
		}
	}
	return metadata
}
