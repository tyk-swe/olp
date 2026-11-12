package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/providers/connection"
)

func (p *Prober) certifyBedrockIngress(ctx context.Context, t Target, model string, tuple connection.Capability, maxEventBytes int) error {
	upstream := t.Connection.Transport().Model(model)
	if upstream == "" {
		upstream = model
	}
	base := "/model/" + url.PathEscape(upstream) + "/"
	if tuple.Operation == "bedrock_invoke" {
		body, family := bedrockInvokeProbe(upstream)
		if family == "" {
			return &Failure{Reason: CapabilityUnavailable, Detail: "InvokeModel ingress supports only qualified Anthropic Claude, Titan embeddings, and Titan image models."}
		}
		if tuple.Mode == "streaming" {
			if family != wire.Anthropic {
				return &Failure{Reason: CapabilityUnavailable, Detail: "InvokeModelWithResponseStream ingress supports only qualified Anthropic Claude models."}
			}
			return p.bedrockProbe(ctx, t, base+"invoke-with-response-stream", true, body, func(data []byte) error {
				return readBedrockProbeStream(data, maxEventBytes)
			})
		}
		return p.bedrockProbe(ctx, t, base+"invoke", false, body, func(data []byte) error {
			if family == wire.Anthropic || family == wire.BedrockEmbeddings {
				if _, err := protocols.Decode(family, family, data, upstream, ""); err != nil {
					return err
				}
				return nil
			}
			return validateBedrockImageProbe(data)
		})
	}
	body, _ := json.Marshal(map[string]any{
		"messages":        []any{map[string]any{"role": "user", "content": []any{map[string]any{"text": "Reply with OK."}}}},
		"inferenceConfig": map[string]any{"maxTokens": 16},
	})
	if tuple.Mode == "streaming" {
		return p.bedrockProbe(ctx, t, base+"converse-stream", true, body, func(data []byte) error {
			return readBedrockProbeStream(data, maxEventBytes)
		})
	}
	return p.bedrockProbe(ctx, t, base+"converse", false, body, func(data []byte) error {
		_, err := protocols.Decode(wire.Bedrock, wire.Bedrock, data, upstream, "")
		return err
	})
}

func (p *Prober) bedrockProbe(ctx context.Context, t Target, path string, stream bool, body []byte, validate func([]byte) error) error {
	var header http.Header
	if stream {
		header = http.Header{"Accept": {"application/vnd.amazon.eventstream"}}
	}
	resp, err := p.do(ctx, t, request{method: http.MethodPost, path: path, body: body, header: header})
	if err != nil {
		return err
	}
	status, data := resp.status, resp.body
	if status != http.StatusOK {
		return statusError(t, status, data)
	}
	if err := validate(data); err != nil {
		return &Failure{Reason: ProviderProtocolError, Detail: "The upstream did not satisfy the requested native codec contract."}
	}
	return nil
}

func bedrockInvokeProbe(model string) ([]byte, wire.Format) {
	switch {
	case strings.Contains(model, "anthropic.claude"):
		return []byte(`{"anthropic_version":"bedrock-2023-05-31","max_tokens":16,"messages":[{"role":"user","content":[{"type":"text","text":"Reply with OK."}]}]}`), wire.Anthropic
	case strings.HasPrefix(model, "amazon.titan-embed-text-"):
		return []byte(`{"inputText":"Reply with OK."}`), wire.BedrockEmbeddings
	case strings.HasPrefix(model, "amazon.titan-image-generator-"):
		return []byte(`{"taskType":"TEXT_IMAGE","textToImageParams":{"text":"A small blue square."},"imageGenerationConfig":{"numberOfImages":1,"width":512,"height":512}}`), wire.ImageGeneration
	}
	return nil, ""
}

func validateBedrockImageProbe(data []byte) error {
	var encodedWire struct {
		Images []string `json:"images"`
		Error  *string  `json:"error"`
	}
	if json.Unmarshal(data, &encodedWire) != nil || encodedWire.Error != nil && *encodedWire.Error != "" || len(encodedWire.Images) == 0 {
		return errors.New("invalid Titan image response")
	}
	return nil
}

func readBedrockProbeStream(data []byte, limit int) error {
	reader := bytes.NewReader(data)
	terminal := false
	for {
		message, err := protocols.ReadBedrockEvent(reader, limit)
		if err != nil {
			if errors.Is(err, io.EOF) {
				if terminal {
					return nil
				}
				return errors.New("Bedrock probe ended before its native terminal event")
			}
			return err
		}
		observation, err := protocols.ObserveBedrockEvent(&message, limit)
		if err != nil {
			return err
		}
		if observation.Failed {
			return errors.New("Bedrock probe returned a native failure")
		}
		terminal = terminal || observation.Terminal
	}
}
