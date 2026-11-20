package generationrefs

import (
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
)

type Reference struct{ Pointer, Kind, ID string }

var errContainer = errors.New("container execution is outside the container lifecycle and file-access contract")

func InRequest(source oif.Document, family wire.Format) ([]Reference, error) {
	switch family {
	case wire.Gemini, wire.GeminiStream:
		return geminiReferences(source.Root())
	case wire.Responses:
		return responsesReferences(source.Root())
	}
	return nil, nil
}

func named(pointer, kind string, value oif.Value) ([]Reference, error) {
	if value.Kind() == oif.Absent || value.Kind() == oif.Null {
		return nil, nil
	}
	id, ok := value.Text()
	if !ok || id == "" {
		return nil, errors.New("resource reference must be a nonempty string")
	}
	return []Reference{{pointer, kind, id}}, nil
}

func geminiReferences(root oif.Value) ([]Reference, error) {
	contents, _ := root.Lookup("contents")
	for _, content := range contents.Elements() {
		parts, _ := content.Lookup("parts")
		if err := callerHosted(parts); err != nil {
			return nil, err
		}
	}
	system, _ := root.Lookup("systemInstruction")
	parts, _ := system.Lookup("parts")
	if err := callerHosted(parts); err != nil {
		return nil, err
	}
	cache, _ := root.Lookup("cachedContent")
	return named("/cachedContent", "context_cache", cache)
}

func callerHosted(parts oif.Value) error {
	for _, part := range parts.Elements() {
		fileData, _ := part.Lookup("fileData")
		fileURI, _ := fileData.Lookup("fileUri")
		if uri, valid := fileURI.Text(); valid && uri != "" && providerManaged(uri) {
			return errors.New("provider-managed Gemini file references have no qualified generation ownership contract")
		}
	}
	return nil
}

func providerManaged(uri string) bool {
	if strings.HasPrefix(strings.TrimPrefix(uri, "/"), "files/") {
		return true
	}
	parsed, _ := url.Parse(uri)
	if parsed == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSuffix(parsed.Hostname(), "."), "generativelanguage.googleapis.com") ||
		strings.EqualFold(parsed.Scheme, "gs") || strings.EqualFold(parsed.Scheme, "s3")
}

func responsesReferences(root oif.Value) ([]Reference, error) {
	conversation, err := conversationReference(root)
	if err != nil {
		return nil, err
	}
	input, _ := root.Lookup("input")
	files, err := inputReferences(input)
	if err != nil {
		return nil, err
	}
	tools, _ := root.Lookup("tools")
	stores, err := toolReferences(tools)
	if err != nil {
		return nil, err
	}
	return slices.Concat(conversation, files, stores), nil
}

func conversationReference(root oif.Value) ([]Reference, error) {
	conversation, present := root.Lookup("conversation")
	if !present || conversation.Kind() == oif.Null {
		return nil, nil
	}
	if conversation.Kind() != oif.Object {
		return named("/conversation", "conversation", conversation)
	}
	id, _ := conversation.Lookup("id")
	if id.Kind() == oif.Absent || id.Kind() == oif.Null {
		return nil, errors.New("conversation needs an identifier")
	}
	return named("/conversation/id", "conversation", id)
}

func inputReferences(input oif.Value) ([]Reference, error) {
	var out []Reference
	for i, item := range input.Elements() {
		refs, err := itemReferences(item, "/input/"+strconv.Itoa(i))
		if err != nil {
			return nil, err
		}
		out = append(out, refs...)
	}
	return out, nil
}

func itemReferences(item oif.Value, path string) ([]Reference, error) {
	kind, _ := item.Lookup("type")
	name, _ := kind.Text()
	if _, hasID := item.Lookup("id"); wire.IsResponsesItemReference(name, hasID) {
		return nil, errors.New("item_reference is not qualified for generation")
	}
	if name == "code_interpreter_call" {
		return nil, errContainer
	}
	own, err := fileReference(item, path)
	if err != nil {
		return nil, err
	}
	field, carries := partsField(name)
	if !carries {
		return own, nil
	}
	content, _ := item.Lookup(field)
	carried, err := partReferences(content, path+"/"+field)
	if err != nil {
		return nil, err
	}
	return slices.Concat(own, carried), nil
}

func partsField(itemType string) (string, bool) {
	switch itemType {
	case "function_call_output", "custom_tool_call_output", "computer_call_output":
		return "output", true
	case "", "message":
		return "content", true
	}
	return "", false
}

func partReferences(content oif.Value, path string) ([]Reference, error) {
	if content.Kind() == oif.Object {
		return fileReference(content, path)
	}
	var out []Reference
	for i, part := range content.Elements() {
		refs, err := fileReference(part, path+"/"+strconv.Itoa(i))
		if err != nil {
			return nil, err
		}
		out = append(out, refs...)
	}
	return out, nil
}

func fileReference(part oif.Value, path string) ([]Reference, error) {
	kind, _ := part.Lookup("type")
	switch name, _ := kind.Text(); name {
	case "input_file", "input_image", "computer_screenshot":
		value, _ := part.Lookup("file_id")
		return named(path+"/file_id", resources.KindFile, value)
	}
	return nil, nil
}

func toolReferences(tools oif.Value) ([]Reference, error) {
	var out []Reference
	for i, tool := range tools.Elements() {
		kind, _ := tool.Lookup("type")
		name, _ := kind.Text()
		if name == "code_interpreter" {
			return nil, errContainer
		}
		if name != "file_search" {
			continue
		}
		stores, err := searchedStores(tool, "/tools/"+strconv.Itoa(i)+"/vector_store_ids")
		if err != nil {
			return nil, err
		}
		out = append(out, stores...)
	}
	return out, nil
}

func searchedStores(tool oif.Value, path string) ([]Reference, error) {
	ids, present := tool.Lookup("vector_store_ids")
	if !present || ids.Kind() != oif.Array || len(ids.Elements()) == 0 {
		return nil, errors.New("file_search requires owned vector stores")
	}
	var out []Reference
	for i, id := range ids.Elements() {
		refs, err := named(path+"/"+strconv.Itoa(i), "vector_store", id)
		if err != nil {
			return nil, err
		}
		out = append(out, refs...)
	}
	return out, nil
}
