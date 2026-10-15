package connectors

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
)

func TestCloneOwnsNestedConfiguration(t *testing.T) {
	config := Config{
		SemanticHeaders: map[string]string{"Header": "original"}, QuerySettings: map[string]string{"query": "original"}, PluginOptions: map[string]string{"option": "original"}, CredentialHeaders: []string{"Header"},
		Models:            map[string]json.RawMessage{"model": json.RawMessage(`{"context_length":123}`)},
		OperationDefaults: map[string]DefaultSet{"generation": {Values: map[string]json.RawMessage{"temperature": json.RawMessage(`0.2`)}, NativeOptions: map[string]json.RawMessage{"top_p": json.RawMessage(`0.9`)}}},
		Bindings:          map[string]Binding{"model": {Defaults: map[string]DefaultSet{"generation": {Values: map[string]json.RawMessage{"max_tokens": json.RawMessage(`12`)}}}}},
		Network:           &egress.ConnectionOptions{MaxConnsPerHost: new(3), ConnectTimeoutMS: new(int64(100))},
	}
	clone := config.Clone()
	clone.SemanticHeaders["Header"] = "changed"
	clone.QuerySettings["query"] = "changed"
	clone.PluginOptions["option"] = "changed"
	clone.CredentialHeaders[0] = "changed"
	clone.Models["model"][0] = '['
	clone.OperationDefaults["generation"].Values["temperature"][0] = '1'
	clone.OperationDefaults["generation"].NativeOptions["top_p"][0] = '1'
	clone.Bindings["model"].Defaults["generation"].Values["max_tokens"][0] = '9'
	*clone.Network.MaxConnsPerHost = 8
	*clone.Network.ConnectTimeoutMS = 200
	if config.SemanticHeaders["Header"] != "original" || config.QuerySettings["query"] != "original" || config.PluginOptions["option"] != "original" || config.CredentialHeaders[0] != "Header" || config.Models["model"][0] != '{' || string(config.OperationDefaults["generation"].Values["temperature"]) != "0.2" || string(config.OperationDefaults["generation"].NativeOptions["top_p"]) != "0.9" || string(config.Bindings["model"].Defaults["generation"].Values["max_tokens"]) != "12" || *config.Network.MaxConnsPerHost != 3 || *config.Network.ConnectTimeoutMS != 100 {
		t.Fatal("clone shared mutable configuration")
	}
}
