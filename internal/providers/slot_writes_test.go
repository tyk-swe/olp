package providers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/platform/secretstore"
)

func TestWriteSlotRejectsAmbiguousCredentialAndVersion(t *testing.T) {
	t.Parallel()

	const (
		providerID = "018f1f77-9823-7b8a-b847-3658e91e4d01"
		slotID     = "018f1f77-9823-7b8a-b847-3658e91e4d02"
		versionID  = "018f1f77-9823-7b8a-b847-3658e91e4d03"
	)

	cases := map[string]struct {
		body      string
		wantField string
	}{
		"pasted credential with credential_version_id": {
			body:      `{"slot":{"name":"primary","credential_version_id":"` + versionID + `"},"credential":"sk-new-secret"}`,
			wantField: "credential",
		},
		"external reference with credential_version_id": {
			body:      `{"slot":{"name":"primary","credential_version_id":"` + versionID + `"},"credential_reference":{"store":"gcp","secret_id":"projects/demo/secrets/api-key","version":"1"}}`,
			wantField: "credential_reference",
		},
		"pasted credential with external reference": {
			body:      `{"slot":{"name":"primary"},"credential":"sk-new-secret","credential_reference":{"store":"gcp","secret_id":"projects/demo/secrets/api-key","version":"1"}}`,
			wantField: "credential_reference",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/"+providerID+"/credential-slots/"+slotID, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.SetPathValue("provider_id", providerID)
			req.SetPathValue("slot_id", slotID)

			_, err := (&slotService{}).writeSlot(req, authz.Principal{})
			problem, ok := httpkit.ProblemFor(err)
			if !ok {
				t.Fatalf("unexpected error %v", err)
			}
			if problem.Status != http.StatusUnprocessableEntity || problem.Code != "validation_failed" || problem.Field != tc.wantField {
				t.Fatalf("got status %d code %q field %q, want status %d code %q field %q",
					problem.Status, problem.Code, problem.Field, http.StatusUnprocessableEntity, "validation_failed", tc.wantField)
			}
		})
	}
}

func TestValidateSlotCredentialSourceAllowsUnambiguousInputs(t *testing.T) {
	t.Parallel()

	versionID := "018f1f77-9823-7b8a-b847-3658e91e4d03"
	ref := externalCredential(secretstore.Reference{Store: "gcp", SecretID: "projects/demo/secrets/api-key", Version: "1"})
	cases := map[string]struct {
		slot   slotInput
		source credentialSource
	}{
		"existing version only":    {slot: slotInput{CredentialVersionID: &versionID}},
		"pasted credential only":   {source: pastedCredential("sk-secret")},
		"external reference only":  {source: ref},
		"no credential adjustment": {},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := validateSlotCredentialSource(tc.slot, tc.source); err != nil {
				t.Fatalf("validateSlotCredentialSource returned unexpected error: %v", err)
			}
		})
	}
}
