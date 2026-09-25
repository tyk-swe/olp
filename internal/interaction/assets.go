package interaction

import (
	"encoding/json"
	"slices"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations/generation"
)

// FileReferences returns the caller's resource IDs at dialect-owned file
// positions. These values only name candidate bindings; the resource
// authority decides which are real, owned, and currently authorized.
func FileReferences(document oif.Document, dialect generation.Dialect) []string {
	if dialect.Assets == nil {
		return nil
	}
	var out []string
	for _, ref := range dialect.Assets(document) {
		if !ref.Refuse && ref.Text {
			out = append(out, ref.ID)
		}
	}
	return out
}

// admitAssets resolves every dialect-owned provider asset position the target
// dialect surveyed. Positions the grammar marked unqualifiable keep the
// historical refusal; a file reference survives only when the resource
// authority supplied a verified binding for exactly the local ID the caller
// wrote, that binding was committed for this serving identity, and the
// profile admits its upload purpose at this position.
func (t *Template) admitAssets(source generation.Source, prepared oif.Prepared, context Context, receipt *Receipt) (oif.Prepared, []AssetBinding, error) {
	fail := func(path string) error {
		return incompatible("resource_affinity", path, "asset_resource_authority", "Provider asset references require resolved ownership and historical serving authority.")
	}
	if t.target.Assets == nil {
		return prepared, nil, nil
	}
	positions := t.target.Assets(prepared.Document())
	// Unqualified provider-asset forms refuse before any binding is
	// consulted; the first such position in document order fails.
	for _, position := range positions {
		if position.Refuse {
			return prepared, nil, fail(position.Pointer)
		}
	}
	if len(positions) == 0 {
		return prepared, nil, nil
	}
	var changes []oif.Change
	var bindings []AssetBinding
	bound := map[string]bool{}
	for _, position := range positions {
		if !position.Text {
			return prepared, nil, fail(position.Pointer)
		}
		binding, found := context.ResolvedAssets[position.ID]
		if !found || binding.LocalID != position.ID || binding.NativeID == "" || !position.Bindable {
			return prepared, nil, fail(position.Pointer)
		}
		if !slices.Contains(t.profile.FilePurposes, binding.Purpose) {
			return prepared, nil, incompatible("resource_affinity", position.Pointer, "asset_purpose", "The resolved file was not uploaded for use as provider inference input.")
		}
		if binding.Serving != t.serving {
			return prepared, nil, incompatible("resource_affinity", position.Pointer, "serving_affinity", "The resolved resource belongs to a different serving identity.")
		}
		encoded, err := json.Marshal(binding.NativeID)
		if err != nil {
			return prepared, nil, fail(position.Pointer)
		}
		changes = append(changes, oif.Change{Pointer: position.Pointer, Value: string(encoded), Origin: oif.ResourceBinding, Reason: "resolved provider file binding"})
		receipt.Dispositions = append(receipt.Dispositions, Disposition{Field: position.Pointer, Disposition: "bound", Rule: "resolved_resource_binding", Evidence: evidenceNative})
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
	resolved, err := oif.PrepareDestination(source.Request, destination, document, oif.ResourceBinding, "resolved provider file binding")
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
