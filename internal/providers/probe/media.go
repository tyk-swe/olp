package probe

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/providerconfig"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/staging"
	"github.com/tyk-swe/olp/internal/vendors"
)

// Media certifies without billed work wherever the upstream allows it. Custom
// hosts cannot use unrelated chat success or a self-reported model list as
// media evidence. Vertex, Bedrock and Azure offer no costless proof, so their
// media certifies by the smallest real call of the operation.
func (p *Prober) certifyNativeMedia(ctx context.Context, t Target, model string, tuple connection.Capability) error {
	vendor := providerconfig.Value(t.Connection.Document().Options.VendorID)
	if mediaByCall(t.Connection.Document().Kind, vendor, tuple.Operation) {
		return p.certifyMediaCall(ctx, t, model, tuple.Operation)
	}
	if reviewedMedia(t.Connection.Document().Kind, vendor, tuple.Operation) {
		return p.certifyVendorMedia(ctx, t, model, vendor)
	}
	endpoint, err := url.Parse(providerconfig.Value(t.Connection.Document().Endpoint))
	if err != nil || t.Connection.Document().Kind != connectors.KindOpenAI || t.Connection.Document().AuthMode != connectors.AuthAPIKey || len(t.Credential) == 0 ||
		len(t.Connection.Document().Options.CredentialHeaders) != 0 || endpoint.Scheme != "https" ||
		endpoint.Hostname() != "api.openai.com" || (endpoint.Port() != "" && endpoint.Port() != "443") ||
		strings.TrimRight(endpoint.EscapedPath(), "/") != "/v1" {
		return &Failure{Reason: CapabilityUnavailable, Detail: "Media certification requires the official OpenAI endpoint and an API key."}
	}
	return p.certifyListedModel(ctx, t, model)
}

var mediaByCallOperations = map[connectors.Kind][]media.Operation{
	connectors.KindVertexAI:    {media.OpImageGeneration, media.OpSpeech, media.OpTranscription},
	connectors.KindBedrock:     {media.OpImageGeneration, media.OpSpeech},
	connectors.KindAzureOpenAI: {media.OpImageGeneration, media.OpSpeech, media.OpTranscription},
}

func mediaByCall(kind connectors.Kind, vendor, operation string) bool {
	op := media.Operation(operation)
	if !slices.Contains(mediaByCallOperations[kind], op) {
		return false
	}
	return op == media.OpImageGeneration || kind == connectors.KindAzureOpenAI || vendors.MediaWire(vendor, operation) != ""
}

func (p *Prober) certifyMediaCall(ctx context.Context, t Target, model, operation string) error {
	transport := t.Connection.Transport()
	if t.Connection.Document().Kind == connectors.KindAzureOpenAI && transport.Hosting() == "azure-v1" {
		return &Failure{Reason: CapabilityUnavailable, Detail: "Azure OpenAI serves media through deployments; its v1 API offers none."}
	}
	probeRequest, failure := mediaProbeRequest(t.Connection.Document().Kind, operation)
	if failure != nil {
		return &Failure{Reason: CapabilityUnavailable, Detail: failure.Message}
	}
	call, _, failure := media.EncodeConfigured(probeRequest, transport, model)
	if failure != nil {
		return &Failure{Reason: CapabilityUnavailable, Detail: failure.Message}
	}
	body, contentType, failure := mediaProbeBody(call.Body, media.SilentWAV(100))
	if failure != nil {
		return &Failure{Reason: CapabilityUnavailable, Detail: failure.Message}
	}
	endpoint, signed, err := media.Resolve(call.Endpoint, transport, model)
	if err != nil {
		return &Failure{Reason: CapabilityUnavailable, Detail: "The media probe endpoint could not be built."}
	}
	resp, err := p.do(ctx, t, request{method: call.Method, path: endpoint, body: body, contentType: contentType, signingService: signed.SigningService})
	if err != nil {
		return err
	}
	status, data := resp.status, resp.body
	if status != http.StatusOK {
		return statusError(t, status, data)
	}
	if mErr := decodeMediaProbe(call.Response, data); mErr != nil {
		return &Failure{Reason: ProviderProtocolError, Detail: "The upstream did not satisfy the requested native codec contract."}
	}
	return nil
}

type mediaProbe struct {
	Model   string `json:"model"`
	Prompt  string `json:"prompt,omitempty"`
	Count   int64  `json:"n,omitempty"`
	Quality string `json:"quality,omitempty"`
	Input   string `json:"input,omitempty"`
	Voice   string `json:"voice,omitempty"`
	Format  string `json:"response_format,omitempty"`
}

func mediaProbeRequest(kind connectors.Kind, operation string) (*media.Request, *media.Error) {
	switch media.Operation(operation) {
	case media.OpImageGeneration:
		probe := mediaProbe{Model: "certification", Prompt: "A small blue square.", Count: 1}
		if kind == connectors.KindAzureOpenAI {
			probe.Quality = "low"
		}
		return decodeProbe(probe, media.DecodeImageGeneration)
	case media.OpSpeech:
		voices := map[connectors.Kind]mediaProbe{connectors.KindBedrock: {Voice: "Joanna"}, connectors.KindVertexAI: {Voice: "Kore", Format: "wav"}}
		probe := cmp.Or(voices[kind], mediaProbe{Voice: "alloy"})
		return decodeProbe(mediaProbe{Model: "certification", Input: "OK", Voice: probe.Voice, Format: probe.Format}, media.DecodeSpeech)
	case media.OpTranscription:
		return media.DecodeTranscription(staging.FormOf([]staging.Field{
			{Name: "model", Value: staging.Text("certification")},
			{Name: "file", Value: staging.Part{Filename: "silence.wav", ContentType: "audio/wav"}},
		}, false))
	}
	return nil, media.Fail(400, "capability_unavailable", "The media operation has no certification probe.")
}

func decodeProbe(probe mediaProbe, decode func([]byte) (*media.Request, *media.Error)) (*media.Request, *media.Error) {
	body, err := json.Marshal(probe)
	if err != nil {
		return nil, media.Fail(400, "capability_unavailable", "The media probe could not be encoded.")
	}
	return decode(body)
}

func mediaProbeBody(body media.Body, silence []byte) ([]byte, string, *media.Error) {
	switch body := body.(type) {
	case media.JSONBody:
		return body, "application/json", nil
	case media.NativeJSON:
		return body, "application/json", nil
	case media.InlineBody:
		encoded, failure := body(func(staging.Part) ([]byte, *media.Error) { return silence, nil })
		return encoded, "application/json", failure
	case media.UploadBody:
		return silence, "audio/wav", nil
	case media.FormBody:
		var form bytes.Buffer
		writer := multipart.NewWriter(&form)
		open := func(staging.Part) (io.ReadCloser, staging.Part, error) {
			return io.NopCloser(bytes.NewReader(silence)), staging.Part{Filename: "silence.wav", ContentType: "audio/wav"}, nil
		}
		if err := media.WriteForm(writer, body, open); err != nil {
			return nil, "", media.Fail(400, "capability_unavailable", "The media probe could not be encoded.")
		}
		return form.Bytes(), writer.FormDataContentType(), nil
	}
	return nil, "", media.Fail(400, "capability_unavailable", "The media probe has no body.")
}

func decodeMediaProbe(response media.Response, data []byte) *media.Error {
	stage := func(b64 string, index int) (*staging.Artifact, *media.Error) {
		if _, err := base64.StdEncoding.DecodeString(b64); err != nil {
			return nil, media.ProtocolError("The provider image payload is not valid base64.")
		}
		return &staging.Artifact{}, nil
	}
	switch reply := response.(type) {
	case media.ImageReply:
		_, failure := reply.Images(data, stage)
		return failure
	case media.InlineAudioReply:
		_, failure := reply.Decode(data)
		return failure
	case media.AudioReply:
		if len(data) == 0 {
			return media.ProtocolError("The upstream returned no audio.")
		}
	case media.TranscriptReply:
		_, failure := reply.Transcript(data)
		return failure
	}
	return nil
}

// The reviewed codec proves the wire, and the vendor's listing or else its
// account probe proves the credential reaches the model. Without a listing, the
// model is the operator's declaration.
func (p *Prober) certifyVendorMedia(ctx context.Context, t Target, model, vendor string) error {
	contract, _ := vendors.Lookup(vendor)
	if contract.AccountProbe == "" {
		return p.certifyListedModel(ctx, t, model)
	}
	status, data, err := p.call(ctx, t, http.MethodGet, "/"+contract.AccountProbe, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return statusError(t, status, data)
	}
	return nil
}
