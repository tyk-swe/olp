package interaction

import (
	"encoding/json"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// fileReference is one dialect-owned position carrying a caller-supplied
// provider or gateway file identifier. Collecting a reference never grants it
// meaning: only positions satisfied by a verified ResolvedAssets binding may
// survive into an effective request.
type fileReference struct {
	pointer string
	value   oif.Value
	kind    string
}

// admittedFileKinds are the only Responses part types whose file_id member a
// resolved binding may occupy. Every other provider-asset position keeps the
// historical refusal; a local ID is never a claim the request document can
// widen into a new position.
var admittedFileKinds = map[string]bool{"input_file": true, "input_image": true, "computer_screenshot": true}

// walkFilePositions visits every dialect-owned file_id position and every
// other provider-asset form, in document order. visit sees each collected
// file reference; refuse is called once per position that remains an
// unqualified provider reference. Either callback may halt the walk by
// returning an error.
func walkFilePositions(document oif.Document, wire openai.Family, visit func(fileReference) error, refuse func(string) error) error {
	openaiFilePart := func(part oif.Value, path, kind string) error {
		if kind != "file" && !admittedFileKinds[kind] {
			return nil
		}
		if value, present := part.Lookup("file_id"); present && value.Kind() != oif.Null {
			if err := visit(fileReference{pointer: path + "/file_id", value: value, kind: kind}); err != nil {
				return err
			}
		}
		if value, present := member(part, "file").Lookup("file_id"); present && value.Kind() != oif.Null {
			if err := visit(fileReference{pointer: path + "/file/file_id", value: value, kind: kind}); err != nil {
				return err
			}
		}
		return nil
	}
	var parts func(oif.Value, string) error
	parts = func(value oif.Value, path string) error {
		for index, part := range value.Elements() {
			at := oif.Pointer(path, strconv.Itoa(index))
			kind := valueText(member(part, "type"))
			switch wire {
			case openai.FamilyChat:
				if err := openaiFilePart(part, at, kind); err != nil {
					return err
				}
			case openai.FamilyAnthropic:
				if kind == "image" || kind == "document" || kind == "container_upload" {
					if value, present := part.Lookup("file_id"); present && value.Kind() != oif.Null {
						if err := visit(fileReference{pointer: at + "/file_id", value: value, kind: kind}); err != nil {
							return err
						}
					}
					source := member(part, "source")
					if value, present := source.Lookup("file_id"); present && value.Kind() != oif.Null {
						if err := visit(fileReference{pointer: at + "/source/file_id", value: value, kind: kind}); err != nil {
							return err
						}
					} else if valueText(member(source, "type")) == "file" {
						if err := refuse(at + "/source"); err != nil {
							return err
						}
					}
				}
				if kind == "tool_result" {
					if err := parts(member(part, "content"), at+"/content"); err != nil {
						return err
					}
				}
			case openai.FamilyGemini, openai.FamilyGeminiStream:
				uri := valueText(member(member(part, "fileData"), "fileUri"))
				if uri != "" {
					parsed, err := url.Parse(uri)
					if err != nil || strings.HasPrefix(strings.TrimPrefix(uri, "/"), "files/") || strings.EqualFold(strings.TrimSuffix(parsed.Hostname(), "."), "generativelanguage.googleapis.com") || strings.EqualFold(parsed.Scheme, "gs") || strings.EqualFold(parsed.Scheme, "s3") {
						if err := refuse(at + "/fileData/fileUri"); err != nil {
							return err
						}
					}
				}
			case openai.FamilyBedrock:
				for _, kind := range []string{"image", "document", "video"} {
					if _, present := member(member(part, kind), "source").Lookup("s3Location"); present {
						if err := refuse(at + "/" + kind + "/source/s3Location"); err != nil {
							return err
						}
					}
				}
				if err := parts(member(member(part, "toolResult"), "content"), at+"/toolResult/content"); err != nil {
					return err
				}
			}
		}
		return nil
	}
	root := document.Root()
	if wire == openai.FamilyResponses {
		// Responses file references live on input items themselves and inside
		// message content or tool-result output parts.
		for index, item := range member(root, "input").Elements() {
			at := oif.Pointer("/input", strconv.Itoa(index))
			kind := valueText(member(item, "type"))
			if err := openaiFilePart(item, at, kind); err != nil {
				return err
			}
			field := "content"
			switch kind {
			case "", "message":
			case "function_call_output", "custom_tool_call_output", "computer_call_output":
				field = "output"
			default:
				continue
			}
			container := member(item, field)
			elements := container.Elements()
			if container.Kind() == oif.Object {
				elements = []oif.Value{container}
			}
			for i, part := range elements {
				partAt := at + "/" + field
				if container.Kind() == oif.Array {
					partAt = oif.Pointer(partAt, strconv.Itoa(i))
				}
				if err := openaiFilePart(part, partAt, valueText(member(part, "type"))); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, field := range []string{"messages", "input", "contents"} {
		for index, message := range member(root, field).Elements() {
			path := "/" + field + "/" + strconv.Itoa(index)
			partName := "content"
			if field == "contents" {
				partName = "parts"
			}
			if err := parts(member(message, partName), path+"/"+partName); err != nil {
				return err
			}
		}
	}
	return nil
}

// FileReferences returns the caller's resource IDs at dialect-owned file
// positions. These values only name candidate bindings; the resource
// authority decides which are real, owned, and currently authorized.
func FileReferences(document oif.Document, wire openai.Family) []string {
	var out []string
	_ = walkFilePositions(document, wire, func(ref fileReference) error {
		if text, ok := ref.value.Text(); ok {
			out = append(out, text)
		}
		return nil
	}, func(string) error { return nil })
	return out
}

// admitAssets resolves every dialect-owned provider asset position. The walk
// still refuses provider-native references outright; a position survives only
// when the resource authority supplied a verified binding for exactly the
// local ID the caller wrote, that binding was committed for this serving
// identity, and the profile admits its upload purpose at this position.
func (t *Template) admitAssets(request *openai.Request, prepared oif.Prepared, context Context, receipt *Receipt) (oif.Prepared, []AssetBinding, error) {
	fail := func(path string) error {
		return incompatible("resource_affinity", path, "asset_resource_authority", "Provider asset references require resolved ownership and historical serving authority.")
	}
	var positions []fileReference
	if err := walkFilePositions(prepared.Document(), t.wire, func(ref fileReference) error {
		positions = append(positions, ref)
		return nil
	}, fail); err != nil {
		return prepared, nil, err
	}
	if len(positions) == 0 {
		return prepared, nil, nil
	}
	var changes []oif.Change
	var bindings []AssetBinding
	bound := map[string]bool{}
	for _, position := range positions {
		ref, ok := position.value.Text()
		if !ok {
			return prepared, nil, fail(position.pointer)
		}
		binding, found := context.ResolvedAssets[ref]
		if !found || binding.LocalID != ref || binding.NativeID == "" {
			return prepared, nil, fail(position.pointer)
		}
		if t.wire != openai.FamilyResponses || !admittedFileKinds[position.kind] {
			return prepared, nil, fail(position.pointer)
		}
		if !slices.Contains(t.profile.FilePurposes, binding.Purpose) {
			return prepared, nil, incompatible("resource_affinity", position.pointer, "asset_purpose", "The resolved file was not uploaded for use as provider inference input.")
		}
		if binding.Serving != t.serving {
			return prepared, nil, incompatible("resource_affinity", position.pointer, "serving_affinity", "The resolved resource belongs to a different serving identity.")
		}
		encoded, err := json.Marshal(binding.NativeID)
		if err != nil {
			return prepared, nil, fail(position.pointer)
		}
		changes = append(changes, oif.Change{Pointer: position.pointer, Value: string(encoded), Origin: oif.ResourceBinding, Reason: "resolved provider file binding"})
		receipt.Dispositions = append(receipt.Dispositions, Disposition{Field: position.pointer, Disposition: "bound", Rule: "resolved_resource_binding", Evidence: evidenceNative})
		if !bound[binding.LocalID] {
			bound[binding.LocalID] = true
			bindings = append(bindings, binding)
		}
	}
	if len(changes) == 0 {
		return prepared, nil, nil
	}
	document, err := oif.Apply(prepared.Document(), changes)
	if err != nil {
		return prepared, nil, incompatible("target_capability", "/request", "resource_binding_overlay", "Resolved resource bindings conflict with the effective request.")
	}
	// The descriptor records every resource the plan now depends on so the
	// prepared document keeps an explicit authority trail, not a JSON claim.
	destination := prepared.Descriptor()
	for _, binding := range bindings {
		destination.Resources = append(destination.Resources, oif.Resource{ID: binding.LocalID, Kind: binding.Kind, Relation: "file_input"})
		if binding.Digest != "" {
			destination.Assets = append(destination.Assets, oif.Asset{ID: binding.LocalID, Digest: binding.Digest, MediaType: binding.MediaType, Size: binding.Size})
		}
	}
	resolved, err := oif.PrepareDestination(request.OIF(), destination, document, oif.ResourceBinding, "resolved provider file binding")
	if err != nil {
		return prepared, nil, incompatible("target_capability", "/request", "resource_binding_overlay", "Resolved resource bindings conflict with the effective request.")
	}
	resolved = resolved.WithProvenance(prepared.Provenance()...)
	for _, change := range changes {
		resolved = resolved.WithProvenance(oif.Provenance{Pointer: change.Pointer, Origin: change.Origin, Reason: change.Reason})
	}
	if !slices.Contains(receipt.Obligations.Effects, "resource_read") {
		receipt.Obligations.Effects = append(receipt.Obligations.Effects, "resource_read")
	}
	return resolved, bindings, nil
}
