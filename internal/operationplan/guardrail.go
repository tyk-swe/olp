package operationplan

import (
	"github.com/tyk-swe/olp/internal/domain/operationregistry"
	"github.com/tyk-swe/olp/internal/domain/operations"
)

// GuardrailInput exposes the operation-owned inspection surface after defaults.
func (p *Plan) GuardrailInput() ([]operations.Text, error) {
	if p.template.codec.InputText == nil {
		return nil, fail("guardrail_surface_unavailable", "/request", "guardrail_input", "The operation has no text inspection contract.")
	}
	texts, err := p.template.codec.InputText(p.effective)
	if err != nil || p.mapping == nil {
		return texts, err
	}
	codec, _ := operationregistry.Lookup(p.source.Descriptor().Dialect.ID)
	if codec.InputText == nil {
		return nil, fail("guardrail_surface_unavailable", "/request", "guardrail_input", "The source operation has no text inspection contract.")
	}
	source, err := codec.InputText(p.source)
	return append(texts, source...), err
}

// GuardrailOutput uses the operation's own result contract. Vectors and other
// opaque payloads must never be mistaken for an empty, successfully inspected
// text response.
func (p *Plan) GuardrailOutput(result Result) ([]operations.Text, error) {
	if p.template.codec.OutputText == nil {
		return nil, fail("guardrail_surface_unavailable", "/result", "guardrail_output", "The operation has no text inspection contract.")
	}
	texts, err := p.template.codec.OutputText(result.Envelope)
	if err != nil || p.mapping == nil {
		return texts, err
	}
	codec, _ := operationregistry.Lookup(p.source.Descriptor().Dialect.ID)
	if codec.OutputText == nil {
		return nil, fail("guardrail_surface_unavailable", "/result", "guardrail_output", "The source operation has no text inspection contract.")
	}
	projected, err := codec.OutputText(result.projected)
	return append(texts, projected...), err
}
