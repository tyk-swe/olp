package operationplan

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
)

// fieldVocabulary is the schema-owned member vocabulary of one registered
// dialect: declared properties, required inputs and defaultable controls.
// Receipts use it so arbitrary caller member names never appear.
func fieldVocabulary(codec operations.Dialect) map[string]bool {
	fields := map[string]bool{"model": true}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	_ = json.Unmarshal(codec.RequestSchema, &schema)
	for name := range schema.Properties {
		fields[name] = true
	}
	for _, name := range schema.Required {
		fields[name] = true
	}
	for name := range codec.Defaults {
		fields[name] = true
	}
	return fields
}

// memberName decodes the top-level member a JSON pointer refers to.
func memberName(pointer string) (string, bool) {
	if len(pointer) < 2 || pointer[0] != '/' {
		return "", false
	}
	segment := pointer[1:]
	if i := strings.IndexByte(segment, '/'); i >= 0 {
		segment = segment[:i]
	}
	if !strings.Contains(segment, "~") {
		return segment, true
	}
	var b strings.Builder
	for i := 0; i < len(segment); i++ {
		if segment[i] != '~' {
			b.WriteByte(segment[i])
			continue
		}
		i++
		if i >= len(segment) || segment[i] != '0' && segment[i] != '1' {
			return "", false
		}
		if segment[i] == '0' {
			b.WriteByte('~')
		} else {
			b.WriteByte('/')
		}
	}
	return b.String(), true
}

// covers reports whether a provenance pointer describes member name or a
// value beneath it.
func covers(pointer, name string) bool {
	top, ok := memberName(pointer)
	return ok && top == name
}

// changeProvenance derives the receipt accounting from the declared changes
// themselves; the two are never allowed to drift.
func changeProvenance(changes []oif.Change) []oif.Provenance {
	out := make([]oif.Provenance, 0, len(changes))
	for _, c := range changes {
		out = append(out, oif.Provenance{Pointer: c.Pointer, Origin: c.Origin, Reason: c.Reason})
	}
	return out
}

// declaredConstruction validates a co-construction contract: re-applying the
// declared changes to the source must reproduce the returned destination
// byte-for-byte. A change the declaration omits, or a declared change whose
// destination value differs, fails before the plan is admitted. This is a
// replay-and-compare over registered declarations, not a solver.
func declaredConstruction(source, constructed oif.Document, declared []oif.Change, scope string) error {
	rebuilt, err := oif.Apply(source, declared)
	if err != nil {
		return fail("fidelity_protocol_violation", scope, "declared_construction", "The registered construction declared a change outside the document contract.")
	}
	if !bytes.Equal(rebuilt.Bytes(), constructed.Bytes()) {
		return fail("fidelity_protocol_violation", scope, "declared_construction", "The registered construction's declared changes do not match its destination.")
	}
	return nil
}

// declaredProjection checks that every realized result-projection change was
// declared by the registered mapping, matching destination field and rule.
func declaredProjection(declared []oif.Disposition, realized []oif.Provenance) error {
	for _, entry := range realized {
		found := false
		for _, disposition := range declared {
			if disposition.Field == "/result"+entry.Pointer && disposition.Rule == entry.Reason {
				found = true
				break
			}
		}
		if !found {
			return fail("fidelity_protocol_violation", "/result", "declared_construction", "The qualified projection applied a change it did not declare.")
		}
	}
	return nil
}

type provenanceKey struct {
	origin oif.Origin
	reason string
}

// relocationPairs finds origin+reason rules that join a removal and an
// introduction: the two scopes of one declared relocation.
func relocationPairs(source, destination oif.Document, declared []oif.Provenance) map[provenanceKey]bool {
	removals, introductions := map[provenanceKey]bool{}, map[provenanceKey]bool{}
	for _, entry := range declared {
		_, before := source.Lookup(entry.Pointer)
		_, after := destination.Lookup(entry.Pointer)
		key := provenanceKey{entry.Origin, entry.Reason}
		switch {
		case before && !after:
			removals[key] = true
		case !before && after:
			introductions[key] = true
		}
	}
	out := map[provenanceKey]bool{}
	for key := range removals {
		out[key] = introductions[key]
	}
	return out
}

func changeKind(source, destination oif.Document, entry oif.Provenance, relocation map[provenanceKey]bool) string {
	if entry.Origin == oif.IdentityBinding || entry.Origin == oif.ResourceBinding {
		return "bound"
	}
	if relocation[provenanceKey{entry.Origin, entry.Reason}] {
		return "relocated"
	}
	_, before := source.Lookup(entry.Pointer)
	_, after := destination.Lookup(entry.Pointer)
	switch {
	case before && !after:
		return "removed"
	case !before && after:
		return "introduced"
	default:
		return "mapped"
	}
}

func defaultOrigin(origin oif.Origin) bool {
	return origin == oif.ProviderDefault || origin == oif.ModelDefault || origin == oif.OperationDefault
}

// memberDispositions accounts for every declared change and every untouched
// member between the caller source and the effective destination. Source
// members keep their scope; introduced members name the destination scope.
func memberDispositions(source, effective oif.Document, declared []oif.Provenance, vocab map[string]bool, codecEvidence, mappingEvidence string) []oif.Disposition {
	relocation := relocationPairs(source, effective, declared)
	byMember := map[string][]oif.Provenance{}
	for _, entry := range declared {
		if name, ok := memberName(entry.Pointer); ok {
			byMember[name] = append(byMember[name], entry)
		}
	}
	out := []oif.Disposition{}
	emit := func(pointer, disposition, rule, evidence string) {
		field := "/$native"
		if name, ok := memberName(pointer); ok && vocab[name] {
			field = pointer
		}
		out = append(out, oif.Disposition{Field: field, Disposition: disposition, Rule: rule, Evidence: evidence})
	}
	emitChange := func(entry oif.Provenance) {
		if defaultOrigin(entry.Origin) {
			return // absent-only defaults carry their own disposition
		}
		evidence := codecEvidence
		if entry.Origin == oif.QualifiedMapping || entry.Origin == oif.ExplicitTransform || entry.Origin == oif.LegacyMapping {
			evidence = mappingEvidence
		}
		emit(entry.Pointer, changeKind(source, effective, entry, relocation), entry.Reason, evidence)
	}
	for _, member := range source.Root().Members() {
		entries := byMember[member.Name]
		if len(entries) == 0 {
			emit(oif.Pointer("", member.Name), "preserved", "native_source_identity", codecEvidence)
			continue
		}
		for _, entry := range entries {
			emitChange(entry)
		}
	}
	for _, member := range effective.Root().Members() {
		if _, present := source.Root().Lookup(member.Name); present {
			continue
		}
		for _, entry := range byMember[member.Name] {
			emitChange(entry)
		}
	}
	// A declared change must never disappear: pointers that do not resolve to
	// a member (for example a root replacement) still account for themselves
	// under the collapsed field label.
	for _, entry := range declared {
		if _, ok := memberName(entry.Pointer); !ok {
			emitChange(entry)
		}
	}
	return out
}

// resultDispositions accounts for realized result-projection changes in
// /result scope. Realized pointers are registered-schema names: qualified
// projections are validated against their declared contract, and native
// bindings admit only identity rules.
func resultDispositions(source, output oif.Document, realized []oif.Provenance, codecEvidence, mappingEvidence string) []oif.Disposition {
	relocation := relocationPairs(source, output, realized)
	out := make([]oif.Disposition, 0, len(realized))
	for _, entry := range realized {
		evidence := codecEvidence
		if entry.Origin == oif.QualifiedMapping || entry.Origin == oif.ExplicitTransform || entry.Origin == oif.LegacyMapping {
			evidence = mappingEvidence
		}
		out = append(out, oif.Disposition{Field: "/result" + entry.Pointer, Disposition: changeKind(source, output, entry, relocation), Rule: entry.Reason, Evidence: evidence})
	}
	return out
}

func compactDispositions(input []oif.Disposition) []oif.Disposition {
	out := make([]oif.Disposition, 0, min(len(input), 128))
	seen := map[oif.Disposition]bool{}
	for _, entry := range input {
		if !seen[entry] {
			seen[entry] = true
			out = append(out, entry)
		}
	}
	return out
}
