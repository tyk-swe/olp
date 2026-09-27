// Package plugins installs provider plugins and runs them confined (ADR 0005).
//
// A plugin is a WebAssembly module that speaks the ABI in sdk/plugin/abi. OLP
// stores it by the SHA-256 digest of the module, and it can't be used until an
// owner approves the origins its manifest declares. Runtime runs plugin code on
// wazero within memory and time limits and grants it only a clock, randomness
// and redacted logging.
package plugins

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// Limits bound every instance of a plugin module.
type Limits struct {
	// Memory bounds an instance's linear memory, in bytes.
	Memory uint32
	// Time bounds one call, including instantiating the module for it.
	Time time.Duration
}

// DefaultLimits are the limits OLP runs plugins within.
var DefaultLimits = Limits{Memory: 64 << 20, Time: 10 * time.Second}

// maxMessage bounds every message crossing the ABI in either direction.
const maxMessage = 1 << 20

const wasmPage = 64 << 10

var (
	i32 = api.ValueTypeI32
	i64 = api.ValueTypeI64
)

// Runtime runs confined plugin modules. It is safe for concurrent use.
type Runtime struct {
	engine wazero.Runtime
	limits Limits
	log    *slog.Logger
}

// NewRuntime starts a runtime whose plugin calls stay within limits and log to
// log.
func NewRuntime(ctx context.Context, limits Limits, log *slog.Logger) (*Runtime, error) {
	// The interpreter compiles a Go-built module several times faster than
	// wazero's compiler and needs no executable memory. Plugin code runs for
	// manifests, grant steps and signing, never per stream event, so install
	// latency matters more than execution speed.
	config := wazero.NewRuntimeConfigInterpreter().
		WithMemoryLimitPages(limits.Memory / wasmPage).
		WithCloseOnContextDone(true)
	r := &Runtime{engine: wazero.NewRuntimeWithConfig(ctx, config), limits: limits, log: log}
	// WASI supplies the clock, randomness and output streams. With no
	// preopened directories, arguments or environment it reaches nothing else.
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, r.engine); err != nil {
		r.engine.Close(ctx)
		return nil, err
	}
	_, err := r.engine.NewHostModuleBuilder(abi.HostModule).
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(hostCall), []api.ValueType{i32, i32}, []api.ValueType{i64}).
		Export(abi.HostCall).
		Instantiate(ctx)
	if err != nil {
		r.engine.Close(ctx)
		return nil, err
	}
	return r, nil
}

// Close releases the runtime and every module compiled in it.
func (r *Runtime) Close(ctx context.Context) error { return r.engine.Close(ctx) }

// Module is a compiled plugin module built for the ABI this runtime serves.
type Module struct {
	// Digest is the lowercase hexadecimal SHA-256 digest of the module.
	Digest   string
	runtime  *Runtime
	compiled wazero.CompiledModule
}

// Load compiles a module and checks that it is a provider plugin built for
// this ABI version.
func (r *Runtime) Load(ctx context.Context, module []byte) (*Module, error) {
	sum := sha256.Sum256(module)
	compiled, err := r.engine.CompileModule(ctx, module)
	if err != nil {
		return nil, refuse(CodeModuleInvalid, "The upload is not a WebAssembly module OLP can run.")
	}
	m := &Module{Digest: hex.EncodeToString(sum[:]), runtime: r, compiled: compiled}
	if err = m.checkABI(ctx); err != nil {
		compiled.Close(ctx)
		return nil, err
	}
	return m, nil
}

// Close releases the compiled module.
func (m *Module) Close(ctx context.Context) error { return m.compiled.Close(ctx) }

// Inspect loads a module and returns the manifest it declares. It refuses a
// module that is not a plugin, was built for another ABI version, declares an
// invalid manifest or names a dialect OLP doesn't have.
func (r *Runtime) Inspect(ctx context.Context, module []byte) (abi.Manifest, error) {
	m, err := r.Load(ctx, module)
	if err != nil {
		return abi.Manifest{}, err
	}
	defer m.Close(context.WithoutCancel(ctx))
	var raw json.RawMessage
	if err = m.Call(ctx, Call{Method: abi.MethodManifest}, &raw); err != nil {
		if reported, ok := errors.AsType[*abi.Error](err); ok {
			return abi.Manifest{}, refuse(CodeManifestInvalid, "The plugin reported no manifest: "+reported.Message)
		}
		return abi.Manifest{}, err
	}
	return decodeManifest(raw)
}

func (m *Module) checkABI(ctx context.Context) error {
	exports := m.compiled.ExportedFunctions()
	if _, command := exports["_start"]; command {
		return refuse(CodeModuleInvalid, "The module is a WASI command. Build the plugin as a reactor, with -buildmode=c-shared.")
	}
	if !hasSignature(exports[abi.ExportVersion], nil, []api.ValueType{i32}) {
		return refuse(CodeModuleInvalid, "The module does not export "+abi.ExportVersion+", so it is not a provider plugin.")
	}
	for _, imported := range m.compiled.ImportedFunctions() {
		module, name, _ := imported.Import()
		switch {
		case module == wasi_snapshot_preview1.ModuleName:
		case module == abi.HostModule && name == abi.HostCall && hasSignature(imported, []api.ValueType{i32, i32}, []api.ValueType{i64}):
		case module == abi.HostModule:
			return refuse(CodeABIUnsupported, fmt.Sprintf("The module imports %s.%s, which plugin ABI %d does not provide. It was built for another ABI version.", module, name, abi.Version))
		default:
			return refuse(CodeModuleInvalid, fmt.Sprintf("The module imports %s.%s, which OLP does not provide.", module, name))
		}
	}
	return m.run(ctx, abi.ExportVersion, nil, func(ctx context.Context, instance api.Module) error {
		results, err := instance.ExportedFunction(abi.ExportVersion).Call(ctx)
		if err != nil {
			return err
		}
		if version := int32(results[0]); version != abi.Version {
			return refuse(CodeABIUnsupported, fmt.Sprintf("The module was built for plugin ABI %d; this OLP runs ABI %d.", version, abi.Version))
		}
		if !hasSignature(exports[abi.ExportAlloc], []api.ValueType{i32}, []api.ValueType{i32}) ||
			!hasSignature(exports[abi.ExportCall], []api.ValueType{i32, i32}, []api.ValueType{i64}) ||
			instance.Memory() == nil {
			return refuse(CodeModuleInvalid, fmt.Sprintf("The module does not implement plugin ABI %d: it needs its memory, %s and %s exported.", abi.Version, abi.ExportAlloc, abi.ExportCall))
		}
		return nil
	})
}

func hasSignature(f api.FunctionDefinition, params, results []api.ValueType) bool {
	return f != nil && bytes.Equal(f.ParamTypes(), params) && bytes.Equal(f.ResultTypes(), results)
}

// Call is one invocation of plugin code.
type Call struct {
	Method string
	Params any
	// Provider is the provider a call on behalf of one serves: the profile it
	// uses and its option values, which the plugin receives with the call.
	Provider *abi.Provider
	// Secrets are values the call hands the plugin. OLP redacts them from
	// everything the plugin logs and from failures it reports.
	Secrets []string
}

// Call serves call on a fresh instance of the module and decodes its result
// into result. A call that exceeds its limits, traps or exits fails with an
// *Error and leaves nothing behind; a failure the plugin reports is an
// *abi.Error.
func (m *Module) Call(ctx context.Context, call Call, result any) error {
	request, err := call.request()
	if err != nil {
		return err
	}
	return m.run(ctx, call.Method, call.Secrets, func(ctx context.Context, instance api.Module) error {
		ptr, err := lend(ctx, instance, request)
		if err != nil {
			return err
		}
		results, err := instance.ExportedFunction(abi.ExportCall).Call(ctx, uint64(ptr), uint64(len(request)))
		if err != nil {
			return err
		}
		data, err := borrow(instance, results[0])
		if err != nil {
			return err
		}
		var response abi.Response
		if err = json.Unmarshal(data, &response); err != nil {
			return refuse(CodeFailed, "The plugin answered with something other than a JSON response.")
		}
		if response.Error != nil {
			out := callOutput(ctx)
			return &abi.Error{Code: out.redact(response.Error.Code), Message: out.redact(response.Error.Message)}
		}
		if result == nil {
			return nil
		}
		decoder := json.NewDecoder(bytes.NewReader(response.Result))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(result); err != nil {
			return refuse(CodeFailed, "The plugin's result does not match the "+call.Method+" result.")
		}
		return nil
	})
}

// request encodes the call as the request the plugin serves.
func (c Call) request() ([]byte, error) {
	params, err := json.Marshal(c.Params)
	if err != nil {
		return nil, err
	}
	return json.Marshal(abi.Request{Method: c.Method, Params: params, Provider: c.Provider})
}

// run instantiates the module for one call and runs use within the
// runtime's limits. The instance is discarded afterwards, so a failed call
// leaves no state for the next.
func (m *Module) run(ctx context.Context, method string, secrets []string, use func(context.Context, api.Module) error) error {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, m.runtime.limits.Time)
	defer cancel()
	out := newOutput(m.runtime.log.With("plugin_digest", m.Digest, "plugin_method", method), secrets)
	defer out.close()
	ctx = context.WithValue(ctx, outputKey{}, out)
	config := wazero.NewModuleConfig().
		WithName("").
		WithStartFunctions("_initialize").
		WithSysWalltime().
		WithSysNanotime().
		WithRandSource(rand.Reader).
		WithStdout(out.stream("stdout")).
		WithStderr(out.stream("stderr"))
	instance, err := m.runtime.engine.InstantiateModule(ctx, m.compiled, config)
	if err == nil {
		err = use(ctx, instance)
		instance.Close(context.WithoutCancel(ctx))
	}
	if err == nil || isReported(err) {
		return err
	}
	if parent.Err() != nil {
		return parent.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return refuse(CodeTimedOut, fmt.Sprintf("The plugin exceeded its %s time limit.", m.runtime.limits.Time))
	}
	return refuse(CodeFailed, "The plugin stopped: it trapped, exited or exhausted its memory limit.")
}

// isReported reports whether err already says why a call failed: OLP refused
// something the plugin did, or the plugin reported a failure itself.
func isReported(err error) bool {
	_, refused := errors.AsType[*Error](err)
	_, reported := errors.AsType[*abi.Error](err)
	return refused || reported
}

// hostCall serves the ABI's host_call import: one capability request from the
// plugin.
func hostCall(ctx context.Context, instance api.Module, stack []uint64) {
	var response abi.Response
	message, ok := instance.Memory().Read(uint32(stack[0]), uint32(stack[1]))
	var request abi.Request
	switch {
	case !ok:
		panic(refuse(CodeFailed, "The plugin passed OLP a buffer outside its memory."))
	case len(message) > maxMessage || json.Unmarshal(message, &request) != nil:
		response.Error = &abi.Error{Code: abi.CodeInvalidRequest, Message: "Capability requests are JSON calls of at most 1 MiB."}
	case request.Method == abi.CapabilityLog:
		var record abi.LogRecord
		if json.Unmarshal(request.Params, &record) != nil {
			response.Error = &abi.Error{Code: abi.CodeInvalidRequest, Message: "A log request carries a log record."}
			break
		}
		callOutput(ctx).record(record)
	default:
		response.Error = &abi.Error{Code: abi.CodeUnknownMethod, Message: "OLP grants this call no capability named " + request.Method + "."}
	}
	data, _ := json.Marshal(response)
	ptr, err := lend(ctx, instance, data)
	if err != nil {
		panic(err)
	}
	stack[0] = abi.Pack(ptr, uint32(len(data)))
}

// lend writes a message into a buffer the plugin allocates for it.
func lend(ctx context.Context, instance api.Module, message []byte) (uint32, error) {
	results, err := instance.ExportedFunction(abi.ExportAlloc).Call(ctx, uint64(len(message)))
	if err != nil {
		return 0, err
	}
	ptr := uint32(results[0])
	if !instance.Memory().Write(ptr, message) {
		return 0, refuse(CodeFailed, "The plugin allocated a buffer outside its memory.")
	}
	return ptr, nil
}

// borrow copies a message the plugin returned, packed, out of its memory.
func borrow(instance api.Module, packed uint64) ([]byte, error) {
	ptr, size := abi.Unpack(packed)
	if size > maxMessage {
		return nil, refuse(CodeFailed, "The plugin's response exceeds 1 MiB.")
	}
	data, ok := instance.Memory().Read(ptr, size)
	if !ok {
		return nil, refuse(CodeFailed, "The plugin returned a buffer outside its memory.")
	}
	return bytes.Clone(data), nil
}
