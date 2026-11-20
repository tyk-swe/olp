package generationrefs

import (
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
)

type Reference struct{ Pointer, Kind, ID string }

func InRequest(source oif.Document, family wire.Format) ([]Reference, error) {
	var out []Reference
	add := func(pointer, kind string, value oif.Value) error {
		if value.Kind() == oif.Absent || value.Kind() == oif.Null {
			return nil
		}
		id, ok := value.Text()
		if !ok || id == "" {
			return errors.New("resource reference must be a nonempty string")
		}
		out = append(out, Reference{pointer, kind, id})
		return nil
	}
	root := source.Root()
	if family == wire.Gemini || family == wire.GeminiStream {
		checkParts := func(parts oif.Value) error {
			for _, part := range parts.Elements() {
				fileData, _ := part.Lookup("fileData")
				fileURI, _ := fileData.Lookup("fileUri")
				uri, valid := fileURI.Text()
				if !valid || uri == "" {
					continue
				}
				parsed, _ := url.Parse(uri)
				providerFile := strings.HasPrefix(strings.TrimPrefix(uri, "/"), "files/")
				if parsed != nil {
					providerFile = providerFile || strings.EqualFold(strings.TrimSuffix(parsed.Hostname(), "."), "generativelanguage.googleapis.com") || strings.EqualFold(parsed.Scheme, "gs") || strings.EqualFold(parsed.Scheme, "s3")
				}
				if providerFile {
					return errors.New("provider-managed Gemini file references have no qualified generation ownership contract")
				}
			}
			return nil
		}
		contents, _ := root.Lookup("contents")
		for _, content := range contents.Elements() {
			parts, _ := content.Lookup("parts")
			if err := checkParts(parts); err != nil {
				return nil, err
			}
		}
		system, _ := root.Lookup("systemInstruction")
		parts, _ := system.Lookup("parts")
		if err := checkParts(parts); err != nil {
			return nil, err
		}
		cache, _ := root.Lookup("cachedContent")
		if err := add("/cachedContent", "context_cache", cache); err != nil {
			return nil, err
		}
		return out, nil
	}
	if family != wire.Responses {
		return nil, nil
	}
	conversation, present := root.Lookup("conversation")
	if present && conversation.Kind() != oif.Null {
		path := "/conversation"
		if conversation.Kind() == oif.Object {
			conversation, _ = conversation.Lookup("id")
			path += "/id"
			if conversation.Kind() == oif.Absent || conversation.Kind() == oif.Null {
				return nil, errors.New("conversation needs an identifier")
			}
		}
		if err := add(path, "conversation", conversation); err != nil {
			return nil, err
		}
	}
	input, _ := root.Lookup("input")
	filePart := func(part oif.Value, path string) error {
		kind, _ := part.Lookup("type")
		name, _ := kind.Text()
		if name == "input_file" || name == "input_image" || name == "computer_screenshot" {
			value, _ := part.Lookup("file_id")
			return add(path+"/file_id", resources.KindFile, value)
		}
		return nil
	}
	for i, item := range input.Elements() {
		kind, _ := item.Lookup("type")
		_, hasID := item.Lookup("id")
		if name, _ := kind.Text(); wire.IsResponsesItemReference(name, hasID) {
			return nil, errors.New("item_reference is not qualified for generation")
		}
		if name, _ := kind.Text(); name == "code_interpreter_call" {
			return nil, errors.New("container execution is outside the container lifecycle and file-access contract")
		}
		path := "/input/" + strconv.Itoa(i)
		if err := filePart(item, path); err != nil {
			return nil, err
		}
		name, _ := kind.Text()
		field := "content"
		if name == "function_call_output" || name == "custom_tool_call_output" || name == "computer_call_output" {
			field = "output"
		} else if name != "" && name != "message" {
			continue
		}
		content, _ := item.Lookup(field)
		if content.Kind() == oif.Object {
			if err := filePart(content, path+"/"+field); err != nil {
				return nil, err
			}
		}
		for j, part := range content.Elements() {
			if err := filePart(part, path+"/"+field+"/"+strconv.Itoa(j)); err != nil {
				return nil, err
			}
		}
	}
	tools, _ := root.Lookup("tools")
	for i, tool := range tools.Elements() {
		kind, _ := tool.Lookup("type")
		if name, _ := kind.Text(); name == "code_interpreter" {
			return nil, errors.New("container execution is outside the container lifecycle and file-access contract")
		}
		if name, _ := kind.Text(); name != "file_search" {
			continue
		}
		ids, present := tool.Lookup("vector_store_ids")
		if !present || ids.Kind() != oif.Array || len(ids.Elements()) == 0 {
			return nil, errors.New("file_search requires owned vector stores")
		}
		for j, id := range ids.Elements() {
			if err := add("/tools/"+strconv.Itoa(i)+"/vector_store_ids/"+strconv.Itoa(j), "vector_store", id); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}
