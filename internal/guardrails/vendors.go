package guardrails

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"
)

// namedVendor describes an explicit wire contract. Parameters are validated
// before publication; endpoint, credentials and transport remain shared.
type namedVendor struct {
	validate   func(json.RawMessage) error
	evaluate   func(context.Context, *http.Client, ExternalConfig, externalCredential, string, Segment) (Verdict, error)
	transforms bool
	phases     []string
}

func isModelGuardrail(kind string) bool {
	return kind == "model" || kind == "semantic_guard" || kind == "decision_model"
}

func lookupNamedVendor(kind string) (namedVendor, bool) {
	for _, group := range []map[string]namedVendor{namedVendorsA, namedVendorsB, namedVendorsC, namedVendorsCore} {
		if vendor, ok := group[kind]; ok {
			return vendor, true
		}
	}
	return namedVendor{}, false
}

func decodeVendorParameters(raw json.RawMessage, into any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	return decodeConfig(raw, into)
}

func vendorJSON(ctx context.Context, client *http.Client, endpoint string, body any, credential externalCredential, header, prefix string) ([]byte, error) {
	return externalJSON(ctx, client, endpoint, body, func(req *http.Request, _ []byte) error {
		if credential.APIKey == "" || strings.ContainsAny(credential.APIKey, "\r\n\x00") {
			return errEvaluation
		}
		req.Header.Set(header, prefix+credential.APIKey)
		return nil
	})
}

func vendorBoolean(blocked bool, category string) Verdict {
	if blocked {
		return Verdict{Action: Block, Categories: []string{category}}
	}
	return Verdict{Action: "pass"}
}

func vendorReplacement(segment Segment, replacement, category string) (Verdict, error) {
	if !utf8.ValidString(replacement) || len(replacement) > 4096 || segment.Text == "" {
		return Verdict{}, errEvaluation
	}
	verdict := vendorBoolean(true, category)
	verdict.Redactions = []Redaction{{Segment: segment.ID, Start: 0, End: len(segment.Text), Replacement: replacement}}
	return verdict, nil
}

func supportsTransformation(kind string) bool {
	if vendor, ok := lookupNamedVendor(kind); ok {
		return vendor.transforms
	}
	switch kind {
	case "builtin.regex", "builtin.pii", "builtin.secrets", "builtin.keywords", "builtin.code_execution", "webhook", "presidio", "bedrock", "plugin":
		return true
	}
	return false
}
