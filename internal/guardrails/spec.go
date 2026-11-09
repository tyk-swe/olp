package guardrails

import (
	"encoding/json"
	"errors"

	"github.com/tyk-swe/olp/internal/guardrails/vendor"
	pluginidentity "github.com/tyk-swe/olp/internal/plugins/identity"
)

type Spec interface {
	References() vendor.References
}

type ModelSpec struct {
	Route string
	Judge Judge
}

type Judge interface{ judge() }

func (ModelRule) judge()    {}
func (SemanticRule) judge() {}

type PluginSpec struct {
	ID      string `json:"plugin_id"`
	Version string `json:"plugin_version"`
}

type builtinSpec struct{ detector }
type webhookSpec struct{ vendor.Connection }
type vendorSpec struct{ vendor.Spec }

func (builtinSpec) References() vendor.References { return vendor.References{} }
func (ModelSpec) References() vendor.References   { return vendor.References{} }
func (PluginSpec) References() vendor.References  { return vendor.References{} }

type kind struct {
	phases     func(Phase) bool
	transforms bool
	model      bool
	compresses bool
	redacts    bool
	parse      func(json.RawMessage) (Spec, error)
}

func (k kind) supports(phase Phase) bool { return k.phases == nil || k.phases(phase) }

var natives = map[Kind]kind{
	"builtin.regex":           {transforms: true, redacts: true, parse: builtin(compileRegex)},
	"builtin.pii":             {transforms: true, redacts: true, parse: builtin(compileEntities(piiPatterns))},
	"builtin.secrets":         {transforms: true, redacts: true, parse: builtin(compileEntities(secretPatterns))},
	"builtin.keywords":        {transforms: true, redacts: true, parse: builtin(compileKeywords)},
	"builtin.code_execution":  {transforms: true, parse: builtin(compileCodeExecution)},
	"builtin.tool_permission": {parse: builtin(compileToolPermission)},
	"builtin.schema":          {parse: builtin(compileSchemaDetector)},
	"builtin.tools":           {parse: builtin(compileTools)},
	"typesafe":                {model: true, parse: builtin(compileTypeSafe)},
	"webhook":                 {transforms: true, parse: parseWebhook},
	"plugin":                  {transforms: true, parse: parsePlugin},
	"model":                   {model: true, parse: parseModel},
	"decision_model":          {model: true, parse: parseModel},
	"semantic_guard":          {model: true, parse: parseSemantic},
}

func lookup(t Kind) (kind, bool) {
	if k, ok := natives[t]; ok {
		return k, true
	}
	info, ok := vendor.Lookup(vendor.Type(t))
	if !ok {
		return kind{}, false
	}
	return kind{phases: info.Supports, transforms: info.Transforms, compresses: info.Compresses, parse: func(raw json.RawMessage) (Spec, error) {
		s, err := vendor.Parse(vendor.Type(t), raw)
		return vendorSpec{s}, err
	}}, true
}

func (k Kind) Redacts() bool { return natives[k].redacts }

func ParseRevision(d Revision) (Spec, error) {
	_, spec, err := parse(d)
	return spec, err
}

func parse(d Revision) (kind, Spec, error) {
	if d.TimeoutMS < 1 || d.TimeoutMS > 300000 || !d.OnError.Valid() {
		return kind{}, nil, errors.New("invalid guardrail timeout or failure policy")
	}
	k, ok := lookup(d.Type)
	if !ok {
		return kind{}, nil, errors.New("unknown guardrail type")
	}
	spec, err := k.parse(d.Configuration)
	if err != nil {
		return kind{}, nil, err
	}
	return k, spec, nil
}

func builtin[D detector](compile func(json.RawMessage) (D, error)) func(json.RawMessage) (Spec, error) {
	return func(raw json.RawMessage) (Spec, error) {
		d, err := compile(raw)
		if err != nil {
			return nil, err
		}
		return builtinSpec{d}, nil
	}
}

func parseWebhook(raw json.RawMessage) (Spec, error) {
	c, err := vendor.ParseConnection(raw)
	if err != nil {
		return nil, err
	}
	return webhookSpec{c}, nil
}

func parsePlugin(raw json.RawMessage) (Spec, error) {
	var p PluginSpec
	if err := decodeConfig(raw, &p); err != nil {
		return nil, err
	}
	if !identifier.MatchString(p.ID) || !pluginidentity.ValidDigest(p.Version) {
		return nil, errors.New("pin an approved plugin version")
	}
	return p, nil
}

func parseModel(raw json.RawMessage) (Spec, error) {
	var c struct {
		Route string     `json:"route"`
		Rule  *ModelRule `json:"rule"`
	}
	if err := decodeConfig(raw, &c); err != nil {
		return nil, err
	}
	if !identifier.MatchString(c.Route) || c.Rule == nil || !categoryID.MatchString(c.Rule.Category) || !json.Valid(c.Rule.Equals) || len(c.Rule.Pointer) > 512 || len(c.Rule.Prompt) > 8192 {
		return nil, errors.New("invalid model guardrail route or verdict rule")
	}
	return ModelSpec{Route: c.Route, Judge: *c.Rule}, nil
}

func parseSemantic(raw json.RawMessage) (Spec, error) {
	var c struct {
		Route    string        `json:"route"`
		Semantic *SemanticRule `json:"semantic"`
	}
	if err := decodeConfig(raw, &c); err != nil {
		return nil, err
	}
	if !identifier.MatchString(c.Route) || c.Semantic == nil {
		return nil, errors.New("semantic guardrails require an embedding route and semantic rule")
	}
	if err := validateSemantic(*c.Semantic); err != nil {
		return nil, err
	}
	return ModelSpec{Route: c.Route, Judge: *c.Semantic}, nil
}

func detectorOf(d Revision, spec Spec, external Evaluator) detector {
	if b, ok := spec.(builtinSpec); ok {
		return b.detector
	}
	return externalDetector{definition: d, spec: spec, evaluator: external}
}
