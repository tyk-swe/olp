package generationrefs

import (
	"encoding/json"
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

// ProjectResult overlays documented result references, including
// partial stream items. It never interprets function arguments or tool output
// strings as resource identifiers, and cannot adopt an unknown native object.
func ProjectResult(source oif.Document, resolve Resolve) (oif.Document, error) {
	var changes []oif.Change
	bind := func(value oif.Value, pointer, kind string) error {
		if value.Kind() == oif.Absent || value.Kind() == oif.Null {
			return nil
		}
		id, valid := value.Text()
		if !valid || id == "" {
			return resources.ErrContract
		}
		local, err := resolve(kind, id)
		if err != nil || local == "" {
			return resources.ErrNotFound
		}
		changes = append(changes, oif.Change{Pointer: pointer, Value: quoted(local), Origin: oif.ResourceBinding, Reason: "owned generation result reference"})
		return nil
	}
	annotation := func(value oif.Value, pointer string) error {
		kind, _ := value.Lookup("type")
		if name, _ := kind.Text(); name == "container_file_citation" {
			return resources.ErrContract
		}
		if name, _ := kind.Text(); name != "file_citation" {
			return nil
		}
		id, _ := value.Lookup("file_id")
		return bind(id, pointer+"/file_id", resources.KindFile)
	}
	content := func(value oif.Value, pointer string) error {
		annotations, _ := value.Lookup("annotations")
		for i, value := range annotations.Elements() {
			if err := annotation(value, pointer+"/annotations/"+strconv.Itoa(i)); err != nil {
				return err
			}
		}
		return nil
	}
	item := func(value oif.Value, pointer string) error {
		kind, _ := value.Lookup("type")
		if name, _ := kind.Text(); name == "code_interpreter_call" {
			return resources.ErrContract
		}
		if name, _ := kind.Text(); name == "file_search_call" {
			results, _ := value.Lookup("results")
			for i, result := range results.Elements() {
				path := pointer + "/results/" + strconv.Itoa(i)
				for _, ref := range []struct{ field, kind string }{{"file_id", resources.KindFile}, {"vector_store_id", "vector_store"}} {
					id, _ := result.Lookup(ref.field)
					if err := bind(id, path+"/"+ref.field, ref.kind); err != nil {
						return err
					}
				}
			}
		}
		parts, _ := value.Lookup("content")
		for i, part := range parts.Elements() {
			if err := content(part, pointer+"/content/"+strconv.Itoa(i)); err != nil {
				return err
			}
		}
		return nil
	}
	response := func(value oif.Value, pointer string) error {
		conversation, present := value.Lookup("conversation")
		if present && conversation.Kind() != oif.Null {
			path := pointer + "/conversation"
			if conversation.Kind() == oif.Object {
				conversation, _ = conversation.Lookup("id")
				path += "/id"
			}
			if err := bind(conversation, path, "conversation"); err != nil {
				return err
			}
		}
		tools, _ := value.Lookup("tools")
		for i, tool := range tools.Elements() {
			kind, _ := tool.Lookup("type")
			if name, _ := kind.Text(); name != "file_search" {
				continue
			}
			ids, _ := tool.Lookup("vector_store_ids")
			for j, id := range ids.Elements() {
				if err := bind(id, pointer+"/tools/"+strconv.Itoa(i)+"/vector_store_ids/"+strconv.Itoa(j), "vector_store"); err != nil {
					return err
				}
			}
		}
		output, _ := value.Lookup("output")
		for i, value := range output.Elements() {
			if err := item(value, pointer+"/output/"+strconv.Itoa(i)); err != nil {
				return err
			}
		}
		return nil
	}
	root := source.Root()
	if nested, present := root.Lookup("response"); present {
		if err := response(nested, "/response"); err != nil {
			return oif.Document{}, err
		}
	} else if err := response(root, ""); err != nil {
		return oif.Document{}, err
	}
	if value, present := root.Lookup("item"); present {
		if err := item(value, "/item"); err != nil {
			return oif.Document{}, err
		}
	}
	if value, present := root.Lookup("part"); present {
		if err := content(value, "/part"); err != nil {
			return oif.Document{}, err
		}
	}
	if value, present := root.Lookup("annotation"); present {
		if err := annotation(value, "/annotation"); err != nil {
			return oif.Document{}, err
		}
	}
	return oif.Apply(source, changes)
}
