//go:build integration && codecli

package integration_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
)

func TestCodeQualificationDurableResponseReferencesAndGeneratedConfiguration(t *testing.T) {
	f := newCodeFleet(t)
	path := "/api/v1/code/routes/" + f.route["id"].(string) + "/client-config?gateway_url=" + f.gateways[0].PublicOrigin
	configuration := f.h.want(f.owner, "GET", path, nil, nil, 200)
	if configuration["base_url"] != f.gateways[0].PublicOrigin+"/code/qualification" ||
		!bytes.Contains([]byte(configuration["configuration"].(string)), []byte("OLP_API_KEY")) {
		t.Fatal("configuration lost public route or OLP-only authentication")
	}
	f.success(t, 0, "reference-root", "")
	if err := f.gateways[0].Stop(8 * time.Second); err != nil {
		t.Fatal(err)
	}
	f.gateways[0] = f.in.replica("reference-restarted")
	body := []byte(`{"model":"gpt-5.4","stream":true,"previous_response_id":"resp-controlled","input":[]}`)
	for _, conversation := range []string{"reference-root", "reference-child"} {
		response, data, err := f.request(t.Context(), 0, conversation, "", body)
		if err != nil || response.StatusCode != 200 {
			t.Fatalf("durable reference refused after restart: %v %s", err, data)
		}
	}
	bindings := codePublicDecode[[]codemode.Binding](t, f.list("bindings"))
	if len(bindings) != 2 || bindings[0].RootID != bindings[1].RootID || bindings[0].AccountID != bindings[1].AccountID {
		t.Fatal("response continuation did not inherit the durable tree")
	}
	missing := []byte(`{"model":"gpt-5.4","previous_response_id":"never-observed","input":[]}`)
	before := len(f.peer.Requests())
	response, data, err := f.request(t.Context(), 1, "orphan-reference", "", missing)
	if err != nil || response.StatusCode != 409 || !bytes.Contains(data, []byte("code_parent_unresolved")) || len(f.peer.Requests()) != before {
		t.Fatal("unknown reference reached upstream")
	}
	f.h.want(f.owner, "POST", "/api/v1/code/bindings/"+bindings[0].ID+"/retire", nil, idem("reference-retirement"), 200)
	response, _, err = f.request(t.Context(), 1, "retired-reference", "", body)
	if err != nil || response.StatusCode != 410 || len(f.peer.Requests()) != before {
		t.Fatal("reference revived a retired tree")
	}
	f.success(t, 1, "new-reference-root", "")
	before = len(f.peer.Requests())
	response, data, err = f.request(t.Context(), 1, "new-reference-root", "", body)
	if err != nil || response.StatusCode != 409 || !bytes.Contains(data, []byte("code_parent_unresolved")) || len(f.peer.Requests()) != before {
		t.Fatal("ambiguous upstream response ID authorized a continuation")
	}
}
