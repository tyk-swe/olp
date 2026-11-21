package retained

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
)

type retainedControls struct {
	pin      *kernel.Pin
	route    *routing.Route
	owner    string
	endpoint retainedapi.Endpoint
	root     oif.Value
	parent   *resources.Resource
}

// retainedInputMetadata validates controls which can spend under the pinned
// credential. Only the resulting model identities and billing dimensions are
// retained; native source content and grader definitions remain provider-side.
func retainedInputMetadata(p *kernel.Pin, route *routing.Route, owner string, e retainedapi.Endpoint, source oif.Document, parent *resources.Resource) (map[string]any, error) {
	c := retainedControls{pin: p, route: route, owner: owner, endpoint: e, root: source.Root(), parent: parent}
	meta := map[string]any{"upstream_model": p.Model, "contract_revision": retainedapi.Revision}
	for _, facts := range []func() (map[string]any, error){
		c.contextCache, c.containerFile, c.submissionMetadata, c.tuningMethod, c.cloudTuning, c.cacheExpiry, c.evaluation, c.container,
	} {
		kept, err := facts()
		if err != nil {
			return nil, err
		}
		maps.Copy(meta, kept)
	}
	return meta, nil
}

func (c retainedControls) creates(kind string) bool {
	return c.endpoint.Kind == kind && c.endpoint.Action == "create"
}

func (c retainedControls) contextCache() (map[string]any, error) {
	if c.endpoint.Kind != "context_cache" {
		return nil, nil
	}
	serving := c.pin.Provider.Connector().Model(c.pin.Model)
	if c.endpoint.Action != "create" {
		return map[string]any{"serving_model": serving}, nil
	}
	if tools, present := c.root.Lookup("tools"); present && tools.Kind() != oif.Null && (tools.Kind() != oif.Array || len(tools.Elements()) != 0) {
		return nil, errors.New("cached tools are unsupported; declare supported client tools on each generation request")
	}
	for _, field := range []string{"toolConfig", "tool_config"} {
		if config, present := c.root.Lookup(field); present && config.Kind() != oif.Null {
			return nil, errors.New("cached tool configuration has no qualified generation contract")
		}
	}
	return map[string]any{"serving_model": serving, "no_cached_tools": true}, nil
}

func (c retainedControls) containerFile() (map[string]any, error) {
	if !c.creates("container_file") {
		return nil, nil
	}
	var pins struct {
		Tier string `json:"memory_tier"`
	}
	if c.parent == nil || json.Unmarshal(c.parent.Metadata, &pins) != nil || pins.Tier == "" {
		return nil, resources.ErrContract
	}
	return map[string]any{"memory_tier": pins.Tier}, nil
}

func (c retainedControls) submissionMetadata() (map[string]any, error) {
	metadata, present := c.root.Lookup("metadata")
	if !present || metadata.Kind() == oif.Null {
		return nil, nil
	}
	if metadata.Kind() != oif.Object {
		return nil, errors.New("metadata must be an object")
	}
	for _, field := range retainedSubmissionFields() {
		if _, reserved := metadata.Lookup(field); reserved {
			return nil, errors.New("gateway submission metadata is reserved for resource correlation")
		}
	}
	return nil, nil
}

func (c retainedControls) tuningMethod() (map[string]any, error) {
	if !c.creates("fine_tuning_job") {
		return nil, nil
	}
	method, present := c.root.Lookup("method")
	if !present || method.Kind() == oif.Null {
		return nil, nil
	}
	kind, _ := method.Lookup("type")
	text, _ := kind.Text()
	if text != "supervised" && text != "dpo" {
		return nil, errors.New("this tuning method has no supported training-token settlement contract")
	}
	return nil, nil
}

func (c retainedControls) cloudTuning() (map[string]any, error) {
	if !c.creates("vertex_tuning_job") {
		return nil, nil
	}
	if err := c.tuningLabels(); err != nil {
		return nil, err
	}
	if err := c.tuningDatasets(); err != nil {
		return nil, err
	}
	// Custom model weights and additional remote datasets or evaluation
	// sources need their own owner-bound source contracts.
	for _, field := range c.root.Members() {
		if slices.Contains([]string{"customBaseModel", "distillationSpec", "preferenceOptimizationSpec", "evaluationConfig"}, field.Name) && field.Value.Kind() != oif.Null {
			return nil, errors.New("only the route's base model and configured supervised datasets are supported")
		}
	}
	return nil, nil
}

func (c retainedControls) tuningLabels() error {
	labels, present := c.root.Lookup("labels")
	if !present || labels.Kind() == oif.Null {
		return nil
	}
	if labels.Kind() != oif.Object {
		return resources.ErrContract
	}
	for _, name := range retainedSubmissionFields() {
		if _, reserved := labels.Lookup(name); reserved {
			return errors.New("gateway submission labels are reserved")
		}
	}
	return nil
}

func (c retainedControls) tuningDatasets() error {
	data, _ := c.root.Lookup("supervisedTuningSpec")
	training, _ := data.Lookup("trainingDatasetUri")
	uri, ok := training.Text()
	if !ok || !c.pin.Provider.Connector().AllowsTuningDataset(uri) {
		return errors.New("training dataset is outside the connection's configured URI prefixes")
	}
	validation, present := data.Lookup("validationDatasetUri")
	if !present || validation.Kind() == oif.Null {
		return nil
	}
	uri, ok = validation.Text()
	if !ok || !c.pin.Provider.Connector().AllowsTuningDataset(uri) {
		return errors.New("validation dataset is outside the connection's configured URI prefixes")
	}
	return nil
}

func (c retainedControls) cacheExpiry() (map[string]any, error) {
	if c.endpoint.Kind != "context_cache" || c.endpoint.Action != "patch" {
		return nil, nil
	}
	if len(c.root.Members()) == 0 {
		return nil, errors.New("cache expiration update must not be empty")
	}
	for _, field := range c.root.Members() {
		if field.Name != "ttl" && field.Name != "expireTime" && field.Name != "expire_time" {
			return nil, errors.New("only cache expiration can be updated")
		}
	}
	return nil, nil
}

func (c retainedControls) container() (map[string]any, error) {
	if !c.creates("container") {
		return nil, nil
	}
	if _, present := c.root.Lookup("metadata"); present {
		return nil, errors.New("the pinned container API does not accept metadata")
	}
	value, present := c.root.Lookup("memory_limit")
	if !present {
		return map[string]any{"memory_tier": resources.DefaultRetainedContainerMemoryTier}, nil
	}
	tier, ok := value.Text()
	if !ok || !resources.ValidRetainedContainerMemoryTier(tier) {
		return nil, errors.New("unsupported container memory tier")
	}
	return map[string]any{"memory_tier": tier}, nil
}

func (c retainedControls) evaluation() (map[string]any, error) {
	if c.endpoint.Operation != "eval" {
		return nil, nil
	}
	if c.endpoint.Kind == "eval" && c.endpoint.Action == "update" {
		for _, field := range c.root.Members() {
			if field.Name != "name" && field.Name != "metadata" {
				return nil, errors.New("only evaluation name and metadata can be updated")
			}
		}
	}
	prior, err := c.priorGraders()
	if err != nil {
		return nil, err
	}
	if err := validateEvalSource(c.root, c.endpoint); err != nil {
		return nil, err
	}
	models, err := c.declaredModels(prior)
	if err != nil {
		return nil, err
	}
	for _, model := range models {
		if !retainedEvalModelAllowed(c.pin, c.route, c.owner, model) {
			return nil, resources.ErrContract
		}
	}
	serving, err := c.servingModels(models)
	if err != nil {
		return nil, err
	}
	slices.Sort(models)
	return map[string]any{"authorized_models": models, "serving_models": serving}, nil
}

func (c retainedControls) priorGraders() ([]string, error) {
	e := c.endpoint
	if (e.Kind != "eval_run" || e.Action != "create") && (e.Kind != "eval" || e.Action != "update") {
		return nil, nil
	}
	if c.parent == nil || c.parent.Kind != "eval" {
		return nil, resources.ErrContract
	}
	var prior struct {
		Models        []string          `json:"authorized_models"`
		ServingModels map[string]string `json:"serving_models"`
	}
	if json.Unmarshal(c.parent.Metadata, &prior) != nil {
		return nil, resources.ErrContract
	}
	if prior.Models == nil {
		return nil, errors.New("this evaluation definition has no qualified grader authority; recreate it before starting new runs")
	}
	for _, model := range prior.Models {
		native := c.pin.Provider.Connector().Model(model)
		pinned, present := prior.ServingModels[model]
		if present && pinned != native || !present && native != model {
			return nil, errors.New("this evaluation definition has no compatible native grader binding; recreate it before starting new runs")
		}
	}
	return prior.Models, nil
}

func (c retainedControls) declaredModels(prior []string) ([]string, error) {
	models := append([]string{}, prior...)
	err := walkRetainedEvalModels(c.root, "", func(_ string, model string) error {
		if !retainedEvalModelAllowed(c.pin, c.route, c.owner, model) {
			return errors.New("evaluation model is not authorized on this connection and route")
		}
		if !slices.Contains(models, model) {
			models = append(models, model)
		}
		return nil
	})
	return models, err
}

func (c retainedControls) servingModels(models []string) (map[string]string, error) {
	serving := map[string]string{}
	seenNative := map[string]bool{}
	for _, model := range models {
		native := c.pin.Provider.Connector().Model(model)
		if seenNative[native] {
			return nil, errors.New("evaluation aliases must have distinct native models for usage attribution")
		}
		seenNative[native] = true
		serving[model] = native
	}
	return serving, nil
}
