package gateway

import (
	"bytes"
	"context"
	"errors"
	"strconv"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/resources"
)

// projectGenerationReferences overlays documented result references, including
// partial stream items. It never interprets function arguments or tool output
// strings as resource identifiers, and cannot adopt an unknown native object.
func projectGenerationReferences(source oif.Document, resolve func(string, string) (string, error)) (oif.Document, error) {
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
		changes = append(changes, oif.Change{Pointer: pointer, Value: quotedResponseID(local), Origin: oif.ResourceBinding, Reason: "owned generation result reference"})
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

func (s *Server) generationResultResolver(ctx context.Context, x *call, identity resources.NativeIdentity) func(string, string) (string, error) {
	return func(kind, upstream string) (string, error) {
		key := kind + "\x00" + upstream
		if x.generation.resources != nil {
			if bound := x.generation.resources.bound[key]; bound != nil {
				return bound.ID, nil
			}
		}
		if s.Resources == nil {
			return "", resources.ErrNotFound
		}
		identity.Kind, identity.UpstreamID, identity.ParentID = kind, upstream, nil
		resource, err := s.Resources.GetByUpstreamIncludingDeleted(ctx, identity)
		if errors.Is(err, resources.ErrNotFound) && kind == resources.KindFile {
			identity.Kind = resources.KindStrictFile
			resource, err = s.Resources.GetByUpstreamIncludingDeleted(ctx, identity)
		}
		if err != nil {
			return "", err
		}
		if x.generation.resources != nil {
			x.generation.resources.bound[key] = resource
		}
		return resource.ID, nil
	}
}

func (s *Server) mapGenerationResultReferences(ctx context.Context, x *call, identity resources.NativeIdentity, body []byte) ([]byte, error) {
	doc, err := oif.ParseJSON(body, oif.Limits{MaxBytes: int(s.cfg.MaxResponseBytes)})
	if err != nil {
		return nil, err
	}
	projected, err := projectGenerationReferences(doc, s.generationResultResolver(ctx, x, identity))
	if err != nil {
		return nil, err
	}
	return projected.Bytes(), nil
}

func (s *Server) mapGenerationReferenceFrame(ctx context.Context, x *call, identity resources.NativeIdentity, frame []byte) ([]byte, error) {
	i := bytes.Index(frame, []byte("\ndata: "))
	if i < 0 {
		return frame, nil
	}
	payload := bytes.TrimSpace(frame[i+7:])
	if len(payload) == 0 || payload[0] != '{' {
		return frame, nil
	}
	doc, err := oif.ParseJSON(payload, oif.Limits{MaxBytes: int(s.cfg.MaxEventBytes)})
	if err != nil {
		return nil, err
	}
	projected, err := projectGenerationReferences(doc, s.generationResultResolver(ctx, x, identity))
	if err != nil {
		return nil, err
	}
	out := append([]byte(nil), frame[:i+7]...)
	out = append(out, projected.Bytes()...)
	out = append(out, '\n', '\n')
	return out, nil
}
