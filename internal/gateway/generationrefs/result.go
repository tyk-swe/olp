package generationrefs

import (
	"bytes"
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
)

type Resolve func(kind, upstream string) (string, error)

func quoted(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

// Stored input-item collections use the same documented input positions as a
// Responses request. Resolve each native reference under the stored response's
// owner and pin before returning a collection that callers can replay.
func InInputItems(source oif.Document) ([]Reference, error) {
	items, present := source.Root().Lookup("data")
	if source.Root().Kind() != oif.Object || !present || items.Kind() != oif.Array {
		return nil, resources.ErrContract
	}
	payload := []byte(`{"input":` + items.Raw() + `}`)
	request, err := oif.ParseJSON(payload, oif.Limits{MaxBytes: len(payload)})
	if err != nil {
		return nil, err
	}
	return InRequest(request, wire.Responses)
}

func ProjectInputItems(source oif.Document, refs []Reference, resolve Resolve) (oif.Document, error) {
	var changes []oif.Change
	for _, ref := range refs {
		local, err := resolve(ref.Kind, ref.ID)
		if err != nil || local == "" {
			return oif.Document{}, resources.ErrNotFound
		}
		changes = append(changes, oif.Change{Pointer: "/data" + strings.TrimPrefix(ref.Pointer, "/input"), Value: quoted(local), Origin: oif.ResourceBinding, Reason: "owned stored input reference"})
	}
	return oif.Apply(source, changes)
}

// ProjectResult overlays documented result references, including partial
// stream items. It never interprets function arguments or tool output strings
// as resource identifiers, and cannot adopt an unknown native object.
func ProjectResult(source oif.Document, resolve Resolve) (oif.Document, error) {
	changes, err := resolve.result(source.Root())
	if err != nil {
		return oif.Document{}, err
	}
	return oif.Apply(source, changes)
}

func ProjectBody(body []byte, limits oif.Limits, resolve Resolve) ([]byte, error) {
	doc, err := oif.ParseJSON(body, limits)
	if err != nil {
		return nil, err
	}
	projected, err := ProjectResult(doc, resolve)
	if err != nil {
		return nil, err
	}
	return projected.Bytes(), nil
}

func ProjectFrame(frame []byte, limits oif.Limits, resolve Resolve) ([]byte, error) {
	i := bytes.Index(frame, []byte("\ndata: "))
	if i < 0 {
		return frame, nil
	}
	payload := bytes.TrimSpace(frame[i+7:])
	if len(payload) == 0 || payload[0] != '{' {
		return frame, nil
	}
	projected, err := ProjectBody(payload, limits, resolve)
	if err != nil {
		return nil, err
	}
	return slices.Concat(frame[:i+7], projected, []byte("\n\n")), nil
}

func (resolve Resolve) result(root oif.Value) ([]oif.Change, error) {
	response, pointer := wholeResponse(root)
	whole, err := resolve.response(response, pointer)
	if err != nil {
		return nil, err
	}
	item, _ := root.Lookup("item")
	items, err := resolve.item(item, "/item")
	if err != nil {
		return nil, err
	}
	part, _ := root.Lookup("part")
	parts, err := resolve.content(part, "/part")
	if err != nil {
		return nil, err
	}
	annotation, _ := root.Lookup("annotation")
	annotations, err := resolve.annotation(annotation, "/annotation")
	if err != nil {
		return nil, err
	}
	return slices.Concat(whole, items, parts, annotations), nil
}

func wholeResponse(root oif.Value) (oif.Value, string) {
	if nested, present := root.Lookup("response"); present {
		return nested, "/response"
	}
	return root, ""
}

func (resolve Resolve) response(value oif.Value, pointer string) ([]oif.Change, error) {
	conversation, err := resolve.conversation(value, pointer+"/conversation")
	if err != nil {
		return nil, err
	}
	tools, _ := value.Lookup("tools")
	stores, err := resolve.each(tools, pointer+"/tools", resolve.searchedStores)
	if err != nil {
		return nil, err
	}
	output, _ := value.Lookup("output")
	items, err := resolve.each(output, pointer+"/output", resolve.item)
	if err != nil {
		return nil, err
	}
	return slices.Concat(conversation, stores, items), nil
}

func (resolve Resolve) conversation(response oif.Value, pointer string) ([]oif.Change, error) {
	conversation, present := response.Lookup("conversation")
	if !present || conversation.Kind() == oif.Null {
		return nil, nil
	}
	if conversation.Kind() != oif.Object {
		return resolve.bind(conversation, pointer, "conversation")
	}
	id, _ := conversation.Lookup("id")
	return resolve.bind(id, pointer+"/id", "conversation")
}

func (resolve Resolve) searchedStores(tool oif.Value, pointer string) ([]oif.Change, error) {
	kind, _ := tool.Lookup("type")
	if name, _ := kind.Text(); name != "file_search" {
		return nil, nil
	}
	ids, _ := tool.Lookup("vector_store_ids")
	return resolve.each(ids, pointer+"/vector_store_ids", func(id oif.Value, pointer string) ([]oif.Change, error) {
		return resolve.bind(id, pointer, "vector_store")
	})
}

func (resolve Resolve) item(value oif.Value, pointer string) ([]oif.Change, error) {
	kind, _ := value.Lookup("type")
	name, _ := kind.Text()
	if name == "code_interpreter_call" {
		return nil, resources.ErrContract
	}
	found, err := resolve.searched(name, value, pointer)
	if err != nil {
		return nil, err
	}
	parts, _ := value.Lookup("content")
	cited, err := resolve.each(parts, pointer+"/content", resolve.content)
	if err != nil {
		return nil, err
	}
	return slices.Concat(found, cited), nil
}

func (resolve Resolve) searched(name string, item oif.Value, pointer string) ([]oif.Change, error) {
	if name != "file_search_call" {
		return nil, nil
	}
	results, _ := item.Lookup("results")
	return resolve.each(results, pointer+"/results", resolve.searchResult)
}

func (resolve Resolve) searchResult(result oif.Value, pointer string) ([]oif.Change, error) {
	file, _ := result.Lookup("file_id")
	files, err := resolve.bind(file, pointer+"/file_id", resources.KindFile)
	if err != nil {
		return nil, err
	}
	store, _ := result.Lookup("vector_store_id")
	stores, err := resolve.bind(store, pointer+"/vector_store_id", "vector_store")
	if err != nil {
		return nil, err
	}
	return slices.Concat(files, stores), nil
}

func (resolve Resolve) content(value oif.Value, pointer string) ([]oif.Change, error) {
	annotations, _ := value.Lookup("annotations")
	return resolve.each(annotations, pointer+"/annotations", resolve.annotation)
}

func (resolve Resolve) annotation(value oif.Value, pointer string) ([]oif.Change, error) {
	kind, _ := value.Lookup("type")
	switch name, _ := kind.Text(); name {
	case "container_file_citation":
		return nil, resources.ErrContract
	case "file_citation":
		id, _ := value.Lookup("file_id")
		return resolve.bind(id, pointer+"/file_id", resources.KindFile)
	}
	return nil, nil
}

func (resolve Resolve) each(list oif.Value, pointer string, one func(oif.Value, string) ([]oif.Change, error)) ([]oif.Change, error) {
	var out []oif.Change
	for i, value := range list.Elements() {
		changes, err := one(value, pointer+"/"+strconv.Itoa(i))
		if err != nil {
			return nil, err
		}
		out = append(out, changes...)
	}
	return out, nil
}

func (resolve Resolve) bind(value oif.Value, pointer, kind string) ([]oif.Change, error) {
	if value.Kind() == oif.Absent || value.Kind() == oif.Null {
		return nil, nil
	}
	id, valid := value.Text()
	if !valid || id == "" {
		return nil, resources.ErrContract
	}
	local, err := resolve(kind, id)
	if err != nil || local == "" {
		return nil, resources.ErrNotFound
	}
	return []oif.Change{{Pointer: pointer, Value: quoted(local), Origin: oif.ResourceBinding, Reason: "owned generation result reference"}}, nil
}
