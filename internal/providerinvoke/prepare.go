// Package providerinvoke links semantic preparation to a validated hosting
// profile. Codecs stay protocol/operation-owned; hosting never chooses a chat
// fallback and the gateway remains the sole Attempt/accounting authority.
package providerinvoke

import (
	"bytes"
	"fmt"
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

// Prepare builds a transformed invocation. Automatic providers select the
// destination by provider kind and apply their parameter defaults. Explicit
// profile selection fixes the destination dialect and records every effective
// native default, plugin profile rewrite, forced upstream streaming and
// hosting wrapper in the prepared document. A plugin profile's envelope stays
// outside it: see connectors.Config.WrapRequest.
func Prepare(request *openai.Request, config connectors.Config, model string, parameterDefaults protocols.Object) (Invocation, error) {
	wire := protocols.WireFamily(config.Kind, config.VendorID, request.Family)
	defaults := parameterDefaults
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
	beforeRewrites := prepared.Document()
	prepared, err = config.Rewrite(prepared)
	if err != nil {
		return Invocation{}, profileError(err)
	}
	if err = checkStateRewrites(beforeRewrites, prepared.Document(), wire); err != nil {
		return Invocation{}, profileError(err)
	}
	if prepared, err = config.StreamRequest(prepared); err != nil {
		return Invocation{}, profileError(err)
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

// Hosting rewrites cannot introduce provider retention or replace a reference
// authorized on ingress. Check the effective dialect body before any envelope
// wraps it, including Responses' implicit retention when store is omitted.
func checkStateRewrites(before, after oif.Document, wire openai.Family) error {
	fields := []string{"store"}
	switch wire {
	case openai.FamilyResponses:
		from, _ := before.Root().Lookup("input")
		to, _ := after.Root().Lookup("input")
		if from.Raw() != to.Raw() {
			if err := openai.ValidateResponsesInput(to.Bytes()); err != nil {
				return fmt.Errorf("the plugin profile's rewrite of /input is invalid: %w", err)
			}
		}
		fields = append(fields, "background", "previous_response_id", "conversation")
	case openai.FamilyChat:
	default:
		return nil
	}
	for _, field := range fields {
		from, _ := before.Root().Lookup(field)
		to, _ := after.Root().Lookup(field)
		if from.Raw() == to.Raw() {
			continue
		}
		absent := to.Kind() == oif.Absent || to.Kind() == oif.Null
		switch field {
		case "store":
			if to.Raw() == "false" || absent && (wire == openai.FamilyChat || from.Raw() == "true") {
				continue
			}
		case "background":
			if absent || to.Raw() == "false" {
				continue
			}
		default:
			previous, _ := from.Text()
			if next, ok := to.Text(); absent || ok && (next == "" || next == previous) {
				continue
			}
		}
		return fmt.Errorf("the plugin profile's rewrite of /%s cannot introduce provider state or change a stateful reference", field)
	}
	return nil
}

func Encode(request *openai.Request, config connectors.Config, model string, parameterDefaults protocols.Object) ([]byte, openai.Family, error) {
	invocation, err := Prepare(request, config, model, parameterDefaults)
	if err != nil {
		return nil, "", err
	}
	return invocation.Prepared.Document().Bytes(), invocation.Wire, nil
}

func profileError(err error) error {
	return &openai.RequestError{Code: "unsupported_parameter", Param: "provider_profile", Message: err.Error()}
}
