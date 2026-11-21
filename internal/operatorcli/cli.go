// Package operatorcli implements management commands over the public SDK.
package operatorcli

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"unicode"

	"github.com/tyk-swe/olp/management"
)

type Runner struct {
	Getenv func(string) string
	Out    io.Writer
	Err    io.Writer
}

var groups = map[string]string{"keys": "api-keys", "config": "configuration"}

var (
	errInvalidJSONInput = errors.New("input file must contain JSON")
	errLargeJSONInput   = errors.New("JSON input exceeds 4 MiB or cannot be read")
)

// Handles identifies client command groups without loading process configuration.
func Handles(command string) bool {
	if command == "client-env" || command == "code-setup" || command == "api" {
		return true
	}
	if _, ok := groups[command]; ok {
		return true
	}
	return slices.ContainsFunc(management.Operations(), func(op management.Operation) bool { return op.Group == command })
}

// Run dispatches a generated operation or a configuration promotion workflow.
func (runner Runner) Run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("a management command is required")
	}
	if args[0] == "client-env" {
		return runner.clientEnv(args[1:])
	}
	if args[0] == "code-setup" {
		return runner.codeSetup(ctx, args[1:])
	}
	if len(args) < 2 || args[1] == "help" || args[1] == "--help" {
		return runner.help(args[0])
	}
	if args[0] == "config" {
		return runner.configuration(ctx, args[1:])
	}
	jsonErrors := runner.Getenv("OLP_CLI_ERROR_FORMAT") == "json"
	failure := runner.generated(ctx, args, jsonErrors)
	if failure == nil {
		return nil
	}
	if !jsonErrors {
		return failure.cause
	}
	return failure
}

func (runner Runner) generated(ctx context.Context, args []string, jsonErrors bool) *commandError {
	group := args[0]
	if alias, ok := groups[group]; ok {
		group = alias
	}
	var found []management.Operation
	for _, operation := range management.Operations() {
		if group == "api" && operation.Name == args[1] || operation.Group == group && (commandName(operation) == args[1] || operation.Name == args[1]) {
			found = append(found, operation)
		}
	}
	if len(found) != 1 {
		return commandFailure(management.InvalidInput, errors.New("unknown or ambiguous command; use olp "+args[0]+" help for contract operation IDs"))
	}
	options, positional, err := parseOptions(args[2:])
	if err != nil {
		return commandFailure(management.InvalidInput, err)
	}
	if err := checkOptions(options, "body-file", "if-match", "idempotency-key", "output", "format", "path", "query"); err != nil {
		return commandFailure(management.InvalidInput, err)
	}
	format := options.one("format")
	if format != "" && format != "json" && (format != "csv" || group != "usage") {
		return commandFailure(management.InvalidInput, errors.New("--format must be json, or csv for usage"))
	}
	op := found[0]
	input, err := generatedArguments(op, options, positional)
	if err != nil {
		return commandFailure(management.InvalidInput, err)
	}
	if path := options.one("body-file"); path != "" {
		body, err := readJSON(path)
		if errors.Is(err, errInvalidJSONInput) || errors.Is(err, errLargeJSONInput) {
			return commandFailure(management.InvalidInput, err)
		}
		if err != nil {
			return commandFailure(management.Local, err)
		}
		input.Body = body
	}
	client, err := runner.client()
	if err != nil {
		return commandFailure(management.Local, err)
	}
	result, failure := client.CallClassified(ctx, op, input)
	if failure != nil {
		return callFailure(failure)
	}
	return runner.generatedOutput(result, format, options.one("output"), jsonErrors)
}

func generatedArguments(op management.Operation, options options, positional []string) (management.Arguments, error) {
	input := management.Arguments{Path: map[string]string{}, Query: map[string]any{}, IfMatch: options.one("if-match"), IdempotencyKey: options.one("idempotency-key")}
	pathValues := positional
	for _, p := range op.Parameters {
		if p.In == "path" && len(pathValues) > 0 {
			input.Path[p.Name], pathValues = pathValues[0], pathValues[1:]
		}
	}
	if len(pathValues) != 0 {
		return management.Arguments{}, errors.New("too many positional path parameters")
	}
	for _, pair := range options["path"] {
		name, value, ok := strings.Cut(pair, "=")
		if !ok || name == "" {
			return management.Arguments{}, errors.New("--path requires NAME=VALUE")
		}
		if _, exists := input.Path[name]; exists {
			return management.Arguments{}, errors.New("duplicate path parameter")
		}
		input.Path[name] = value
	}
	for _, pair := range options["query"] {
		name, value, ok := strings.Cut(pair, "=")
		if !ok || name == "" {
			return management.Arguments{}, errors.New("--query requires NAME=VALUE")
		}
		if _, exists := input.Query[name]; exists {
			return management.Arguments{}, errors.New("duplicate scalar query parameter")
		}
		var scalar any = value
		for _, parameter := range op.Parameters {
			if parameter.In != "query" || parameter.Name != name {
				continue
			}
			switch parameter.Type {
			case "integer":
				scalar = json.Number(value)
			case "boolean":
				if value != "true" && value != "false" {
					return management.Arguments{}, fmt.Errorf("--query %s requires true or false", name)
				}
				scalar = value == "true"
			}
		}
		input.Query[name] = scalar
	}
	return input, nil
}

func (runner Runner) generatedOutput(result management.Response, format, path string, jsonErrors bool) *commandError {
	if !jsonErrors {
		runner.writeETags(result)
	}
	data, err := formatResponse(result.Body, format)
	if err != nil {
		return commandFailure(management.InvalidResponse, err)
	}
	if jsonErrors && path == "" {
		brokenPipe := make(chan os.Signal, 1)
		signal.Notify(brokenPipe, syscall.SIGPIPE)
		defer signal.Stop(brokenPipe)
	}
	if err := runner.output(path, data); err != nil {
		return commandFailure(management.Local, err)
	}
	if jsonErrors {
		runner.writeETags(result)
	}
	return nil
}

func (runner Runner) writeETags(result management.Response) {
	if result.ETag != "" {
		fmt.Fprintln(runner.Err, "ETag:", result.ETag)
	}
	if result.PreviousParentETag != "" {
		fmt.Fprintln(runner.Err, "OLP-Previous-Parent-ETag:", result.PreviousParentETag)
	}
	if result.ParentETag != "" {
		fmt.Fprintln(runner.Err, "OLP-Parent-ETag:", result.ParentETag)
	}
}

func formatResponse(body json.RawMessage, format string) ([]byte, error) {
	if format == "csv" {
		return usageCSV(body)
	}
	if len(body) > 0 {
		return append(body, '\n'), nil
	}
	return body, nil
}

func (runner Runner) client() (*management.Client, error) {
	return management.NewClient(runner.Getenv("OLP_MANAGEMENT_URL"), runner.Getenv("OLP_MANAGEMENT_TOKEN_FILE"))
}

func commandName(operation management.Operation) string {
	name := strings.ReplaceAll(operation.Name, "_", "-")
	group := strings.TrimSuffix(operation.Group, "s")
	if strings.HasSuffix(name, "-"+group+"s") {
		name = strings.TrimSuffix(name, "-"+group+"s")
	}
	name = strings.TrimSuffix(name, "-"+group)
	if operation.Group == "usage" {
		name = strings.TrimPrefix(name, "usage-")
	}
	if operation.Group == "routes" {
		switch operation.Name {
		case "create_route_draft":
			name = "create-draft"
		case "get_route_draft":
			name = "get-draft"
		case "list_route_drafts":
			name = "list-drafts"
		case "replace_route_draft":
			name = "update-draft"
		default:
			name = strings.TrimSuffix(name, "-route-draft")
		}
	}
	return name
}

func (runner Runner) help(group string) error {
	if alias, ok := groups[group]; ok {
		group = alias
	}
	fmt.Fprintln(runner.Out, "usage: olp GROUP COMMAND [PATH_VALUES...] [--path NAME=VALUE] [--query NAME=VALUE]")
	fmt.Fprintln(runner.Out, "       [--body-file FILE] [--if-match ETAG] [--idempotency-key KEY] [--output FILE]")
	fmt.Fprintln(runner.Out, "Contract-required mutation keys must be explicit; retry an uncertain write with the same key and payload.")
	if group == "configuration" {
		fmt.Fprintln(runner.Out, "config export [--output FILE]\nconfig plan --file DOCUMENT [--bindings-file FILE] [--code-bindings-file FILE] [--output PLAN]\nconfig apply --plan-file PLAN [--bindings-file FILE] [--code-bindings-file FILE] [--idempotency-key KEY]")
	}
	for _, op := range management.Operations() {
		if group == "api" || op.Group == group {
			fmt.Fprintf(runner.Out, "%s\t%s\t%s %s\n", commandName(op), op.Name, op.Method, op.Path)
		}
	}
	return nil
}

type options map[string][]string

func (opts options) one(name string) string {
	if len(opts[name]) > 0 {
		return opts[name][0]
	}
	return ""
}

func parseOptions(args []string) (options, []string, error) {
	opts, positional := options{}, []string{}
	for i := 0; i < len(args); i++ {
		argument := args[i]
		if argument == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(argument, "--") {
			positional = append(positional, argument)
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(argument, "--"), "=")
		if !hasValue {
			i++
			if i >= len(args) || strings.HasPrefix(args[i], "--") {
				return nil, nil, fmt.Errorf("--%s needs a value", name)
			}
			value = args[i]
		}
		if len(opts[name]) > 0 && name != "path" && name != "query" {
			return nil, nil, fmt.Errorf("duplicate --%s", name)
		}
		opts[name] = append(opts[name], value)
	}
	return opts, positional, nil
}

func checkOptions(opts options, names ...string) error {
	for name := range opts {
		if !slices.Contains(names, name) {
			return fmt.Errorf("unknown option --%s", name)
		}
	}
	return nil
}

func readJSON(path string) (json.RawMessage, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open JSON input file")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (4<<20)+1))
	if err != nil {
		return nil, errors.New("JSON input exceeds 4 MiB or cannot be read")
	}
	if len(data) > 4<<20 {
		return nil, errLargeJSONInput
	}
	if !json.Valid(data) {
		return nil, errInvalidJSONInput
	}
	return data, nil
}

func (runner Runner) output(path string, data []byte) error {
	if path == "" {
		_, err := runner.Out.Write(data)
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".olp-output-*")
	if err != nil {
		return errors.New("cannot create output file")
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func usageCSV(raw json.RawMessage) ([]byte, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, errors.New("usage response must be an object")
	}
	rows := []map[string]json.RawMessage{}
	if items, ok := body["items"]; ok {
		if err := json.Unmarshal(items, &rows); err != nil {
			return nil, errors.New("usage items must be objects")
		}
	} else {
		rows = append(rows, body)
	}
	columns := []string{}
	for _, row := range rows {
		for name := range row {
			if !slices.Contains(columns, name) {
				columns = append(columns, name)
			}
		}
	}
	slices.Sort(columns)
	var out strings.Builder
	writer := csv.NewWriter(&out)
	writer.Write(columns)
	for _, row := range rows {
		values := make([]string, len(columns))
		for i, name := range columns {
			value := row[name]
			if len(value) == 0 || string(value) == "null" {
				continue
			}
			if err := json.Unmarshal(value, &values[i]); err != nil {
				values[i] = string(value)
			}
			trimmed := strings.TrimLeftFunc(values[i], func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) || r == '\uFEFF' })
			if trimmed != "" && strings.ContainsAny(trimmed[:1], "=+-@") {
				values[i] = "'" + values[i]
			}
		}
		writer.Write(values)
	}
	writer.Flush()
	return []byte(out.String()), writer.Error()
}
