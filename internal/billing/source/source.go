package source

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/management/contract"
)

type Kind = contract.BillingImportSource

type Polling struct {
	Today           time.Time
	ImportedThrough *time.Time
	Endpoint        string
	Client          func(endpoint string) (*http.Client, error)
}

type Cloud struct {
	Today      time.Time
	Earliest   time.Time
	Completed  []time.Time
	Resume     *Resume
	Failures   int
	Client     func(endpoint string) (*http.Client, error)
	Auth       *connectors.Auth
	S3Endpoint string
}

type Inputs interface {
	Polling(ctx context.Context) (Polling, error)
	Cloud(ctx context.Context) (Cloud, error)
}

type Scope struct {
	kind      Kind
	canonical json.RawMessage
}

func (s Scope) JSON() json.RawMessage { return s.canonical }

func (s Scope) Digest() [32]byte { return sha256.Sum256(s.canonical) }

func (s Scope) Source() Source { return sources[s.kind] }

type Credential struct {
	kind Kind
	body string
}

type Import struct {
	Scope Scope
	read  func(context.Context, Inputs, func(Period) error) (*Resume, error)
}

func (i Import) Read(ctx context.Context, in Inputs, emit func(Period) error) (*Resume, error) {
	return i.read(ctx, in, emit)
}

type Source struct {
	kind         Kind
	family       string
	providerKind connectors.Kind
	input        func(json.RawMessage) (string, error)
	scope        func(json.RawMessage) (Scope, error)
	credential   func(string) (Credential, error)
	open         func(scope json.RawMessage, credential string) (Import, error)
}

type vendor[S, C, R any] struct {
	family       string
	providerKind connectors.Kind
	input        func(json.RawMessage) (string, error)
	scope        func(json.RawMessage) (S, error)
	credential   func(string) (C, error)
	request      func(Inputs, context.Context) (R, error)
	read         func(context.Context, R, S, C, func(Period) error) (*Resume, error)
}

func bind[S, C, R any](kind Kind, v vendor[S, C, R]) Source {
	canonicalScope := func(raw json.RawMessage) (S, Scope, error) {
		scope, err := v.scope(raw)
		if err != nil {
			return scope, Scope{}, err
		}
		canonical, err := json.Marshal(scope)
		return scope, Scope{kind: kind, canonical: canonical}, err
	}
	return Source{
		kind:         kind,
		family:       v.family,
		providerKind: v.providerKind,
		input:        v.input,
		scope: func(raw json.RawMessage) (Scope, error) {
			_, scope, err := canonicalScope(raw)
			return scope, err
		},
		credential: func(text string) (Credential, error) {
			credential, err := v.credential(text)
			if err != nil {
				return Credential{}, err
			}
			body, err := json.Marshal(credential)
			return Credential{kind: kind, body: string(body)}, err
		},
		open: func(rawScope json.RawMessage, text string) (Import, error) {
			scope, canonical, err := canonicalScope(rawScope)
			if err != nil {
				return Import{}, fail(CodeResponse, fmt.Errorf("invalid %s import configuration: %w", kind, err))
			}
			credential, err := v.credential(text)
			if err != nil {
				return Import{}, fail(CodeCredential, fmt.Errorf("invalid %s import credential: %w", kind, err))
			}
			return Import{Scope: canonical, read: func(ctx context.Context, in Inputs, emit func(Period) error) (*Resume, error) {
				req, err := v.request(in, ctx)
				if err != nil {
					return nil, err
				}
				return v.read(ctx, req, scope, credential, emit)
			}}, nil
		},
	}
}

var sources = map[Kind]Source{
	contract.OpenaiCosts:         bind(contract.OpenaiCosts, openAI),
	contract.AnthropicCostReport: bind(contract.AnthropicCostReport, anthropic),
	contract.AwsCur:              bind(contract.AwsCur, awsCUR),
	contract.AzureCostManagement: bind(contract.AzureCostManagement, azureCostManagement),
	contract.GoogleBigquery:      bind(contract.GoogleBigquery, googleBigQuery),
}

func For(providerKind connectors.Kind) (Source, error) {
	for _, source := range sources {
		if source.providerKind == providerKind {
			return source, nil
		}
	}
	return Source{}, access.Invalid("billing_import", fmt.Sprintf("Billing imports are not supported for provider kind %q.", providerKind))
}

func Lookup(kind Kind) (Source, bool) {
	source, ok := sources[kind]
	return source, ok
}

func (s Source) Kind() Kind { return s.kind }

func (s Source) Scope(raw json.RawMessage) (Scope, error) { return s.scope(raw) }

func (s Source) Credential(raw json.RawMessage) (Credential, error) {
	text, err := s.input(raw)
	if err != nil {
		return Credential{}, err
	}
	return s.credential(text)
}

func (s Source) Secret(text string) (Credential, error) { return s.credential(text) }

func (s Source) Stored(data []byte) (Credential, error) {
	text, err := s.unwrap(data)
	if err != nil {
		return Credential{}, err
	}
	return s.credential(text)
}

func (s Source) Open(scope json.RawMessage, stored []byte) (Import, error) {
	text, err := s.unwrap(stored)
	if err != nil {
		return Import{}, fail(CodeCredential, err)
	}
	return s.open(scope, text)
}

func jsonString(raw json.RawMessage) (string, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return "", access.Invalid("credential", "credential must be a string")
	}
	return text, nil
}

func document(raw json.RawMessage) (string, error) { return string(raw), nil }

type sealed struct {
	Source     Kind            `json:"source"`
	Credential json.RawMessage `json:"credential"`
}

func (c Credential) Seal() ([]byte, error) {
	return json.Marshal(sealed{Source: c.kind, Credential: json.RawMessage(c.body)})
}

type storedDocument struct {
	Source     Kind            `json:"source"`
	Credential json.RawMessage `json:"credential"`
	APIKey     *string         `json:"api_key"`
}

var errIncompatible = errors.New("the stored credential belongs to another billing source")

func (s Source) unwrap(data []byte) (string, error) {
	var stored storedDocument
	if err := json.Unmarshal(data, &stored); err != nil {
		return string(data), nil
	}
	if sources[stored.Source].family != s.family {
		return "", errIncompatible
	}
	switch {
	case stored.Credential != nil:
		return s.input(stored.Credential)
	case stored.APIKey != nil:
		return *stored.APIKey, nil
	default:
		return withoutSource(data)
	}
}

func withoutSource(data []byte) (string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return "", err
	}
	body, err := json.Marshal(maps.Collect(func(yield func(string, json.RawMessage) bool) {
		for key, value := range fields {
			if key != "source" && !yield(key, value) {
				return
			}
		}
	}))
	return string(body), err
}
