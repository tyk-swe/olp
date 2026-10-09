package operatorcli

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/sdk/management"
)

// SavedPlan contains portable, non-secret desired state and the destination
// digest observed at planning. Bindings are supplied separately at each stage.
type SavedPlan struct {
	Version           int                                `json:"version"`
	Destination       string                             `json:"destination"`
	DestinationDigest string                             `json:"destination_digest"`
	Document          json.RawMessage                    `json:"document"`
	Plan              contract.ConfigurationPlanResponse `json:"plan"`
}

func (runner Runner) configuration(ctx context.Context, args []string) error {
	opts, positional, err := parseOptions(args[1:])
	if err != nil {
		return err
	}
	if len(positional) != 0 {
		return errors.New("configuration commands take named file options")
	}
	if err = checkOptions(opts, "file", "plan-file", "bindings-file", "external-bindings-file", "output", "idempotency-key"); err != nil {
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
		saved = SavedPlan{Version: 1, Destination: runner.Getenv("OLP_MANAGEMENT_URL"), DestinationDigest: destination.Digest, Document: data}
	case "apply":
		if opts.one("plan-file") == "" {
			return errors.New("config apply requires --plan-file PLAN")
		}
		data, err := readJSON(opts.one("plan-file"))
		if err != nil {
			return err
		}
		if err = json.Unmarshal(data, &saved); err != nil || saved.Version != 1 || saved.DestinationDigest == "" || saved.Plan.Digest == "" || len(saved.Document) == 0 {
			return errors.New("invalid saved configuration plan")
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
	externalBindings := map[string]json.RawMessage{}
	if path := opts.one("external-bindings-file"); path != "" {
		data, err := readJSON(path)
		if err != nil {
			return err
		}
		if json.Unmarshal(data, &externalBindings) != nil {
			return errors.New("external bindings file must be an object of pinned references")
		}
	}
	request := map[string]any{"document": saved.Document, "expected_digest": saved.DestinationDigest, "secret_bindings": bindings, "external_credential_bindings": externalBindings}
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
	if !reflect.DeepEqual(plan, saved.Plan) {
		return errors.New("destination or desired state changed since planning; compute a new plan")
	}
	result, err = call("apply_configuration", request)
	if err != nil {
		return err
	}
	return runner.output(opts.one("output"), append(result.Body, '\n'))
}
