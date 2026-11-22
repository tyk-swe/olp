package protocols

import (
	"encoding/json"
	"maps"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/openai"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/domain/vendors"
)

func prepareSourceFields(r *wire.Request, vendor, model string, shape vendors.RequestShape, defaults Object, native bool, applied *[]oif.Provenance) (Object, error) {
	var f Object
	if r.Family.Surface() != "openai" {
		f = r.Document()
	}
	sourceDefaults := defaults
	if !native {
		sourceDefaults = nil
	}
	if err := refuseRewriteConflicts(shape, r); err != nil {
		return nil, err
	}
	sourceDefaults = yieldDefaults(shape, r, sourceDefaults)
	if r.Family.Surface() != "openai" {
		mergeDefaultsTracked(f, sourceDefaults, applied)
	} else {
		fields, provenance, err := openai.EncodeFieldsWithProvenance(r, model, sourceDefaults)
		*applied = append(*applied, provenance...)
		if err != nil {
			return nil, err
		}
		f = fields
	}
	if err := validateProfile(vendor, r.Family, f); err != nil {
		return nil, err
	}
	return f, nil
}

func encodeNativeTarget(family wire.Format, vendor, model string, shape vendors.RequestShape, f Object) ([]byte, error) {
	if family.Surface() != "gemini" {
		f["model"] = raw(model)
	} else if present(f["generateContentRequest"]) {
		nested, _ := object(f["generateContentRequest"])
		nested["model"] = raw("models/" + strings.TrimPrefix(model, "models/"))
		f["generateContentRequest"] = raw(nested)
	}
	if family == wire.Rerank {
		return encodeRerank(vendor, f)
	}
	conform(shape, f)
	return json.Marshal(f)
}

func encodeCanonicalTarget(r *wire.Request, encodedWire wire.Format, vendor, model string, shape vendors.RequestShape, f, defaults Object, applied *[]oif.Provenance) ([]byte, wire.Format, error) {
	c, err := decodeCanonical(r.Family, f)
	if err != nil {
		return nil, encodedWire, err
	}
	if len(c.Extensions) > 0 {
		return nil, encodedWire, requestError(c.Extensions[0], "The selected provider cannot preserve request semantics at "+strings.Join(c.Extensions, ", "))
	}
	c.Stream = r.Stream
	if encodedWire == wire.Anthropic && !present(c.Parameters["max_output_tokens"]) {
		if _, explicit := f["max_tokens"]; !explicit {
			c.Parameters["max_output_tokens"] = defaults["max_tokens"]
			if present(defaults["max_tokens"]) {
				*applied = append(*applied, oif.Provenance{Pointer: "/max_tokens", Origin: oif.ProviderDefault, Reason: "absent translated token control inherited native provider default"})
			}
		}
	}
	out, err := encodeCanonical(c, encodedWire, model, r.Family.Operation())
	if err != nil {
		return nil, encodedWire, err
	}
	targetDefaults := Object{}
	maps.Copy(targetDefaults, defaults)
	if present(c.Parameters["max_output_tokens"]) {
		delete(targetDefaults, "max_tokens")
		delete(targetDefaults, "max_completion_tokens")
		delete(targetDefaults, "max_output_tokens")
	}
	for _, key := range []string{"temperature", "top_p", "seed", "stop", "max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if v, ok := f[key]; ok && !present(v) {
			delete(targetDefaults, key)
			if strings.HasPrefix(key, "max_") {
				delete(targetDefaults, "max_tokens")
				delete(targetDefaults, "max_completion_tokens")
				delete(targetDefaults, "max_output_tokens")
			}
		}
	}
	mergeDefaultsTracked(out, targetDefaults, applied)
	conform(shape, out)
	if err := validateProfile(vendor, encodedWire, out); err != nil {
		return nil, encodedWire, err
	}
	encoded, err := json.Marshal(out)
	return encoded, encodedWire, err
}
