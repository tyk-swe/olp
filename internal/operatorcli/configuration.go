package operatorcli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/sdk/management"
)

// SavedPlan contains portable, non-secret desired state and the destination
// digest observed at planning. Bindings are supplied separately at each stage.
type SavedPlan struct {
	Version                int                                `json:"version"`
	Destination            string                             `json:"destination"`
	DestinationDigest      string                             `json:"destination_digest"`
	CodeBindingsDigest     string                             `json:"code_bindings_digest"`
	ExternalBindingsDigest string                             `json:"external_bindings_digest"`
	IntentDigest           string                             `json:"intent_digest"`
	Document               json.RawMessage                    `json:"document"`
	Plan                   contract.ConfigurationPlanResponse `json:"plan"`
}

func (runner Runner) configuration(ctx context.Context, args []string) error {
	opts, positional, err := parseOptions(args[1:])
	if err != nil {
		return err
	}
	if len(positional) != 0 {
		return errors.New("configuration commands take named file options")
	}
	if err = checkOptions(opts, "file", "plan-file", "bindings-file", "external-bindings-file", "code-bindings-file", "output", "idempotency-key"); err != nil {
		return err
	}
	client, err := runner.client()
	if err != nil {
		return err
	}
	call := func(name string, body any) (management.Response, error) {
		operation, ok := management.Lookup(name)
		if !ok {
			return management.Response{}, errors.New("configuration operation missing from contract")
		}
		var encoded []byte
		if body != nil {
			encoded, err = json.Marshal(body)
			if err != nil {
				return management.Response{}, err
			}
		}
		return client.Call(ctx, operation, management.Arguments{Body: encoded, IdempotencyKey: opts.one("idempotency-key")})
	}
	if args[0] == "export" {
		result, err := call("export_configuration", nil)
		if err != nil {
			return err
		}
		return runner.output(opts.one("output"), append(result.Body, '\n'))
	}
	bindings := map[string]string{}
	if path := opts.one("bindings-file"); path != "" {
		data, err := readJSON(path)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(data, &bindings); err != nil {
			return errors.New("bindings file must be an object of secret strings")
		}
	}
	var saved SavedPlan
	switch args[0] {
	case "plan":
		if opts.one("file") == "" {
			return errors.New("config plan requires --file DOCUMENT")
		}
		data, err := readJSON(opts.one("file"))
		if err != nil {
			return err
		}
		var envelope struct {
			Document json.RawMessage `json:"document"`
		}
		if err = json.Unmarshal(data, &envelope); err != nil {
			return errors.New("configuration must be an object")
		}
		if len(envelope.Document) != 0 {
			data = envelope.Document
		}
		exported, err := call("export_configuration", nil)
		if err != nil {
			return err
		}
		var destination contract.ConfigurationExportResponse
		if err = json.Unmarshal(exported.Body, &destination); err != nil || destination.Digest == "" {
			return errors.New("destination export has no digest")
		}
		saved = SavedPlan{Version: 2, Destination: runner.Getenv("OLP_MANAGEMENT_URL"), DestinationDigest: destination.Digest, Document: data}
	case "apply":
		if opts.one("plan-file") == "" {
			return errors.New("config apply requires --plan-file PLAN")
		}
		data, err := readJSON(opts.one("plan-file"))
		if err != nil {
			return err
		}
		if err = json.Unmarshal(data, &saved); err != nil || saved.Version != 2 || saved.DestinationDigest == "" || saved.Plan.Digest == "" || len(saved.Document) == 0 || saved.IntentDigest == "" {
			return errors.New("invalid saved configuration plan; compute a new plan")
		}
		intentDigest, err := savedIntentDigest(saved)
		if err != nil || intentDigest != saved.IntentDigest {
			return errors.New("saved configuration intent changed; compute a new plan")
		}
		if saved.Destination != runner.Getenv("OLP_MANAGEMENT_URL") {
			return errors.New("saved plan was computed for a different destination")
		}
		if len(saved.Plan.Blockers) != 0 || len(saved.Plan.Conflicts) != 0 {
			return errors.New("saved plan has conflicts or blockers")
		}
	default:
		return errors.New("unknown config command; use export, plan or apply")
	}
	externalBindings := map[string]any{}
	if path := opts.one("external-bindings-file"); path != "" {
		data, err := readJSON(path)
		if err != nil {
			return err
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		if decoder.Decode(&externalBindings) != nil {
			return errors.New("external bindings file must be an object of pinned references")
		}
	}
	encodedBindings, err := json.Marshal(externalBindings)
	if err != nil {
		return err
	}
	bindingsHash := sha256.Sum256(encodedBindings)
	bindingsDigest := hex.EncodeToString(bindingsHash[:])
	if args[0] == "plan" {
		saved.ExternalBindingsDigest = bindingsDigest
	} else if saved.ExternalBindingsDigest != bindingsDigest {
		return errors.New("external credential bindings changed since planning; compute a new plan")
	}
	codeBindings, err := readCodeBindings(opts.one("code-bindings-file"))
	if err != nil {
		return err
	}
	encodedCode, err := json.Marshal(codeBindings)
	if err != nil {
		return err
	}
	codeHash := sha256.Sum256(encodedCode)
	codeDigest := hex.EncodeToString(codeHash[:])
	if args[0] == "plan" {
		saved.CodeBindingsDigest = codeDigest
	} else if saved.CodeBindingsDigest != codeDigest {
		return errors.New("code destination bindings changed since planning, compute a new plan")
	}

	request := map[string]any{"document": saved.Document, "expected_digest": saved.DestinationDigest, "secret_bindings": bindings, "external_credential_bindings": externalBindings, "code_bindings": codeBindings}
	if args[0] == "apply" {
		// The server checks current authority, exact replay and destination
		// preconditions in that order. Replanning here would block recovery of
		// our own committed mutation after its response was lost.
		result, err := call("apply_configuration", request)
		if err != nil {
			return err
		}
		return runner.output(opts.one("output"), append(result.Body, '\n'))
	}
	result, err := call("plan_configuration", request)
	if err != nil {
		return err
	}
	var plan contract.ConfigurationPlanResponse
	if err = json.Unmarshal(result.Body, &plan); err != nil || plan.Digest == "" {
		return errors.New("invalid configuration plan response")
	}
	if args[0] == "plan" {
		saved.Plan = plan
		saved.IntentDigest, err = savedIntentDigest(saved)
		if err != nil {
			return err
		}
		data, err := json.MarshalIndent(saved, "", "  ")
		if err != nil {
			return err
		}
		if err = runner.output(opts.one("output"), append(data, '\n')); err != nil {
			return err
		}
		if len(plan.Conflicts) != 0 || len(plan.Blockers) != 0 {
			return errors.New("configuration plan has conflicts or blockers")
		}
		return nil
	}
	return errors.New("unknown config command")
}

// savedIntentDigest checks local consistency, not authorization or authenticity.
// Compact removes formatting but retains native object order and exact scalar
// lexemes inside the raw desired document.
func savedIntentDigest(saved SavedPlan) (string, error) {
	saved.IntentDigest = ""
	var document bytes.Buffer
	if err := json.Compact(&document, saved.Document); err != nil {
		return "", err
	}
	saved.Document = document.Bytes()
	encoded, err := json.Marshal(saved)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func readCodeBindings(path string) (contract.ConfigurationCodeBindings, error) {
	if path == "" {
		return contract.ConfigurationCodeBindings{}, nil
	}
	data, err := readJSON(path)
	if err != nil {
		return contract.ConfigurationCodeBindings{}, err
	}
	var bindings contract.ConfigurationCodeBindings
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&bindings); err != nil {
		return contract.ConfigurationCodeBindings{}, errors.New("code bindings must map credentials, users and api_keys to destination UUIDs")
	}
	return bindings, nil
}
