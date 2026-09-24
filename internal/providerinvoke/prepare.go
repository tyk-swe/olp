// Package providerinvoke links semantic preparation to a validated hosting
// profile. Codecs stay protocol/operation-owned; hosting never chooses a chat
// fallback and the gateway remains the sole Attempt/accounting authority.
package providerinvoke

import (
	"bytes"
	"strings"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

type Invocation struct {
	Prepared oif.Prepared
	Wire     openai.Family
	Defaults []connectors.DefaultProvenance
}

// Prepare retains legacy admission semantics until a route selects the strict
// planner. Explicit profile selection fixes the destination dialect and records
// every effective native default and hosting wrapper in the prepared document.
func Prepare(request *openai.Request, config connectors.Config, model string, legacyDefaults protocols.Object) (Invocation, error) {
	wire := protocols.WireFamily(config.Kind, config.VendorID, request.Family)
	defaults := legacyDefaults
	var origins []connectors.DefaultProvenance
	if config.ProfileID != "" {
		if err := config.ValidateProfile(); err != nil {
			return Invocation{}, profileError(err)
		}
		var err error
		wire, err = config.TargetFamily(request.Family)
		if err != nil {
			return Invocation{}, profileError(err)
		}
		defaults, origins, err = config.DefaultsFor(request.Family.Operation(), model)
		if err != nil {
			return Invocation{}, profileError(err)
		}
	}
	if config.ProfileID != "" && request.Family.Operation() == "embeddings" && wire != request.Family && len(defaults) > 0 {
		for name, raw := range request.Document() {
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				return Invocation{}, &openai.RequestError{Code: "unsupported_parameter", Param: name, Message: "The selected embedding profile cannot qualify explicit null with native defaults."}
			}
		}
	}
	prepared, wire, err := protocols.PrepareTarget(request, wire, config.Kind, config.VendorID, config.Model(model), defaults)
	if err != nil {
		return Invocation{}, err
	}
	if config.ProfileID == "" {
		return Invocation{Prepared: prepared, Wire: wire}, nil
	}
	if request.Family.Operation() == "embeddings" {
		prepared, err = protocols.ApplyEmbeddingDefaults(prepared, wire, defaults)
		if err != nil {
			return Invocation{}, err
		}
	}
	profile := oif.Identity{ID: config.ProfileID, Revision: config.ProfileRevision}
	prepared = prepared.WithProfile(profile)
	applied := []connectors.DefaultProvenance{}
	for _, entry := range prepared.Provenance() {
		if entry.Origin != oif.ProviderDefault {
			continue
		}
		for _, origin := range origins {
			if origin.Pointer == entry.Pointer || strings.HasPrefix(entry.Pointer, origin.Pointer+"/") || strings.HasPrefix(entry.Pointer, "/requests/") && strings.HasSuffix(entry.Pointer, origin.Pointer) {
				origin.Pointer = entry.Pointer
				applied = append(applied, origin)
				break
			}
		}
	}
	body := prepared.Document().Bytes()
	wrapped, err := config.WrapBody(body, wire)
	if err != nil {
		return Invocation{}, profileError(err)
	}
	if !bytes.Equal(body, wrapped) {
		document, err := oif.ParseJSON(wrapped, prepared.Document().Limits())
		if err != nil {
			return Invocation{}, err
		}
		hosted, err := oif.PrepareDestination(prepared.Request(), prepared.Descriptor(), document, oif.Origin("hosting_wrapper"), "model URL binding and required native API revision for "+config.Hosting())
		if err != nil {
			return Invocation{}, err
		}
		prepared = hosted.WithProvenance(prepared.Provenance()...)
	}
	return Invocation{Prepared: prepared, Wire: wire, Defaults: applied}, nil
}

func Encode(request *openai.Request, config connectors.Config, model string, legacyDefaults protocols.Object) ([]byte, openai.Family, error) {
	invocation, err := Prepare(request, config, model, legacyDefaults)
	if err != nil {
		return nil, "", err
	}
	return invocation.Prepared.Document().Bytes(), invocation.Wire, nil
}

func profileError(err error) error {
	return &openai.RequestError{Code: "unsupported_parameter", Param: "provider_profile", Message: err.Error()}
}
