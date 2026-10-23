// Package retainedapi describes provider-retained resource contracts. It does
// not dispatch requests, authorize callers, or turn declarations into codecs.
package retainedapi

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
)

const Revision = "retained-resources-1"

type Family struct {
	Operation  string   `json:"operation"`
	Surface    string   `json:"surface"`
	Path       string   `json:"path"`
	Kind       string   `json:"kind"`
	Identity   string   `json:"identity"`
	Collection string   `json:"collection"`
	Providers  []string `json:"providers"`
	Methods    []string `json:"methods"`
	Children   []Family `json:"children,omitempty"`
	// Metered creation starts deferred work or retained storage. It needs the
	// accounting owner's acceptance contract before it can be dispatched.
	Metered bool `json:"metered"`
}

// Families returns independent copies so callers cannot widen the registry.
func Families() []Family {
	return clone(families)
}

func clone(in []Family) []Family {
	out := slices.Clone(in)
	for i := range out {
		out[i].Methods = slices.Clone(in[i].Methods)
		out[i].Providers = slices.Clone(in[i].Providers)
		out[i].Children = clone(in[i].Children)
	}
	return out
}

var families = []Family{
	{Operation: "fine_tuning", Surface: "openai", Path: "/v1/fine_tuning/jobs", Kind: "fine_tuning_job", Identity: "id", Collection: "data", Providers: []string{"openai", "azure_openai"}, Methods: []string{"create", "list", "get", "cancel"}, Metered: true,
		Children: []Family{{Path: "events", Kind: "fine_tuning_event", Identity: "id", Collection: "data", Methods: []string{"list"}}, {Path: "checkpoints", Kind: "fine_tuning_checkpoint", Identity: "id", Collection: "data", Methods: []string{"list"}}}},
	{Operation: "vector_store", Surface: "openai", Path: "/v1/vector_stores", Kind: "vector_store", Identity: "id", Collection: "data", Providers: []string{"openai", "azure_openai"}, Methods: []string{"create", "list", "get", "update", "delete"}, Metered: true,
		Children: []Family{{Path: "files", Kind: "vector_store_file", Identity: "id", Collection: "data", Methods: []string{"create", "list", "get", "delete"}, Metered: true}, {Path: "file_batches", Kind: "vector_store_batch", Identity: "id", Collection: "data", Methods: []string{"create", "get", "cancel"}, Metered: true}}},
	{Operation: "conversation", Surface: "openai", Path: "/v1/conversations", Kind: "conversation", Identity: "id", Collection: "data", Providers: []string{"openai"}, Methods: []string{"create", "get", "update", "delete"},
		Children: []Family{{Path: "items", Kind: "conversation_item", Identity: "id", Collection: "data", Methods: []string{"create", "list", "get", "delete"}}}},
	{Operation: "container", Surface: "openai", Path: "/v1/containers", Kind: "container", Identity: "id", Collection: "data", Providers: []string{"openai", "azure_openai"}, Methods: []string{"create", "list", "get", "delete"}, Metered: true,
		Children: []Family{{Path: "files", Kind: "container_file", Identity: "id", Collection: "data", Methods: []string{"create", "list", "get", "delete", "content"}, Metered: true}}},
	{Operation: "eval", Surface: "openai", Path: "/v1/evals", Kind: "eval", Identity: "id", Collection: "data", Providers: []string{"openai"}, Methods: []string{"create", "list", "get", "update", "delete"},
		Children: []Family{{Path: "runs", Kind: "eval_run", Identity: "id", Collection: "data", Methods: []string{"create", "list", "get", "cancel", "delete"}, Metered: true, Children: []Family{{Path: "output_items", Kind: "eval_output_item", Identity: "id", Collection: "data", Methods: []string{"list", "get"}}}}}},
	{Operation: "skill", Surface: "anthropic", Path: "/anthropic/v1/skills", Kind: "skill", Identity: "id", Collection: "data", Providers: []string{"anthropic"}, Methods: []string{"create", "list", "get", "delete"},
		Children: []Family{{Path: "versions", Kind: "skill_version", Identity: "version", Collection: "data", Methods: []string{"create", "list", "get", "delete"}}}},
	{Operation: "context_cache", Surface: "gemini", Path: "/gemini/v1beta/cachedContents", Kind: "context_cache", Identity: "name", Collection: "cachedContents", Providers: []string{"gemini"}, Methods: []string{"create", "list", "get", "patch", "delete"}, Metered: true},
	{Operation: "agent_resource", Surface: "gemini", Path: "/gemini/v1beta/agents", Kind: "agent_resource", Identity: "id", Collection: "agents", Providers: []string{"gemini"}, Methods: []string{"create", "list", "get", "delete"}},
}

func Operation(op string) bool {
	return op == "retrieval" || slices.ContainsFunc(families, func(f Family) bool { return f.Operation == op })
}

// Supports is deliberately narrower than the declaration catalogue. A family
// becomes selectable only after its native transport is implemented.
func Supports(kind, operation, surface, mode string) bool {
	return mode == "unary" && kind == "gemini" && operation == "agent_resource" && surface == "gemini"
}

func Kinds() []string {
	var out []string
	var add func([]Family)
	add = func(fs []Family) {
		for _, f := range fs {
			out = append(out, f.Kind)
			add(f.Children)
		}
	}
	add(families)
	return out
}

type Endpoint struct {
	Family
	Action  string `json:"action"`
	Method  string `json:"method"`
	Pattern string `json:"pattern"`
	// Ancestors are in path order. Each segment has a separate typed ID.
	Ancestors []string `json:"ancestors,omitempty"`
}

func Endpoints() []Endpoint {
	var out []Endpoint
	var add func(Family, string, []string)
	add = func(f Family, path string, parents []string) {
		for _, action := range f.Methods {
			verb, suffix := http.MethodGet, ""
			switch action {
			case "create":
				verb = http.MethodPost
			case "list":
			case "get":
				suffix = "/{id}"
			case "update":
				verb, suffix = http.MethodPost, "/{id}"
			case "patch":
				verb, suffix = http.MethodPatch, "/{id}"
			case "delete":
				verb, suffix = http.MethodDelete, "/{id}"
			case "cancel":
				verb, suffix = http.MethodPost, "/{id}/cancel"
			case "content":
				suffix = "/{id}/content"
			}
			out = append(out, Endpoint{Family: f, Action: action, Method: verb, Pattern: path + suffix, Ancestors: slices.Clone(parents)})
		}
		for _, c := range f.Children {
			c.Operation, c.Surface, c.Providers = f.Operation, f.Surface, slices.Clone(f.Providers)
			add(c, path+"/{parent"+strconv.Itoa(len(parents))+"}/"+c.Path, append(slices.Clone(parents), f.Kind))
		}
	}
	for _, f := range Families() {
		add(f, f.Path, nil)
	}
	return out
}

// Rewrite walks only documented resource-bearing fields. User metadata,
// schemas and opaque values are deliberately not interpreted as references.
// Every other id-shaped field must be declared by the codec or is refused.
func Rewrite(source oif.Document, refs map[string]string, resolve func(kind, id string) (string, error)) (oif.Document, error) {
	var changes []oif.Change
	var walk func(oif.Value, string) error
	walk = func(v oif.Value, path string) error {
		switch v.Kind() {
		case oif.Array:
			for i, child := range v.Elements() {
				if err := walk(child, path+"/"+strconv.Itoa(i)); err != nil {
					return err
				}
			}
		case oif.Object:
			for _, field := range v.Members() {
				p := path + "/" + strings.ReplaceAll(strings.ReplaceAll(field.Name, "~", "~0"), "/", "~1")
				if path == "/base_environment/env" {
					// Variable names are user data; each binding still has typed fields.
					if err := walk(field.Value, p); err != nil {
						return err
					}
					continue
				}
				if slices.Contains([]string{"metadata", "attributes", "schema", "json_schema", "parameters", "input_schema"}, field.Name) {
					continue
				}
				kind, reference := refs[p]
				if !reference {
					kind, reference = refs[field.Name]
				}
				if reference && field.Value.Kind() != oif.Null {
					values := []oif.Value{field.Value}
					if field.Value.Kind() == oif.Array {
						values = field.Value.Elements()
					}
					for i, value := range values {
						id, ok := value.Text()
						if !ok || id == "" {
							return errors.New("resource reference must be a nonempty string")
						}
						bound, err := resolve(kind, id)
						if err != nil {
							return err
						}
						pointer := p
						if field.Value.Kind() == oif.Array {
							pointer += "/" + strconv.Itoa(i)
						}
						changes = append(changes, oif.Change{Pointer: pointer, Value: strconv.Quote(bound), Origin: oif.ResourceBinding, Reason: "owner-scoped retained resource"})
					}
					continue
				}
				if (field.Name == "id" || strings.HasSuffix(field.Name, "_id") || strings.HasSuffix(field.Name, "_ids")) && field.Value.Kind() != oif.Null {
					return errors.New("undeclared resource reference: " + field.Name)
				}
				if err := walk(field.Value, p); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(source.Root(), ""); err != nil {
		return oif.Document{}, err
	}
	return oif.Apply(source, changes)
}
