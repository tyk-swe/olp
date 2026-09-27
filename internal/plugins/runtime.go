// Package plugins installs provider plugins and runs them confined (ADR 0005).
//
// A plugin is a WebAssembly module that speaks the ABI in sdk/plugin/abi. OLP
// stores it by the SHA-256 digest of the module, and it can't be used until an
// owner approves the origins its manifest declares. Runtime runs plugin code on
// wazero within memory and time limits and grants it only a clock, randomness,
// redacted logging and, to calls that are granted it, HTTP to its approved
// origins. Where a deployment enables the unconfined tier, an owner may also
// permit an executable in its image as an unconfined plugin, which Unconfined
// runs as a subprocess speaking the ABI over stdio. Host runs installed
// plugins' code by digest.
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
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// Limits bound every instance of a plugin module.
type Limits struct {
	// Memory bounds an instance's linear memory, in bytes.
	Memory uint32
	// Time bounds one call, including waiting for an instance and
	// instantiating the module for it.
	Time time.Duration
	// Stack bounds the stack of one call, in bytes: the parameters, locals
	// and operands of every frame its plugin code has entered and not left.
	Stack uint32
	// Instances bounds how many instances of one module exist at once, and so
	// how many of its calls run at once.
	Instances int
}

// DefaultLimits are the limits OLP runs plugins within.
var DefaultLimits = Limits{Memory: 64 << 20, Time: 10 * time.Second, Stack: 8 << 20, Instances: 4}

// maxMessage bounds every message a plugin returns or sends OLP. OLP's own
// requests carry what they must, such as the body a sign request signs.
const maxMessage = 1 << 20

const wasmPage = 64 << 10

var (
	i32 = api.ValueTypeI32
	i64 = api.ValueTypeI64
)

// An Engine is how a runtime runs plugin code. For the reference plugin, a
// 5 MiB Go module, the interpreter is ready in about 0.4 s and signs a small
// request in about 15 ms; the compiler takes about 3 s and then signs in about
// 2 ms, counting every frame towards the call's stack limit. BenchmarkSign
// measures both.
type Engine int

const (
	// Interpreted interprets modules, which suits a module OLP calls once,
	// such as one being installed.
	Interpreted Engine = iota
	// Compiled compiles modules to machine code, where wazero can, which
	// suits modules OLP keeps and calls per request.
	Compiled
)

// Runtime runs confined plugin modules. It is safe for concurrent use.
type Runtime struct {
	engine wazero.Runtime
	limits Limits
	log    *slog.Logger
	// mu orders loading modules before closing the runtime: wazero does not
	// guard compiling against a runtime closed meanwhile, such as by a
	// process stopping while a Host prepares a plugin.
	mu     sync.RWMutex
	closed bool
}

// NewRuntime starts a runtime whose plugin calls run on engine, stay within
// limits and log to log.
func NewRuntime(ctx context.Context, engine Engine, limits Limits, log *slog.Logger) (*Runtime, error) {
	config := wazero.NewRuntimeConfigInterpreter()
	if engine == Compiled {
		// The compiler where wazero has one for the platform, else the
		// interpreter.
		config = wazero.NewRuntimeConfig()
	}
	// Without reference types, a module has at most one table, which never
	// grows, so declare bounds every table (declared.go).
	config = config.WithCoreFeatures(api.CoreFeaturesV2 &^ api.CoreFeatureReferenceTypes).
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

// Close releases the runtime and every module compiled in it, once loads under
// way end. Loading fails afterwards.
func (r *Runtime) Close(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return r.engine.Close(ctx)
}

// errClosedRuntime fails loading a module on a runtime that was closed.
var errClosedRuntime = errors.New("the plugin runtime is closed")

// Module is a compiled plugin module built for the ABI this runtime serves.
// Its calls share a pool of instances, which Limits.Instances bounds: a call
// takes an idle instance, or instantiates the module if none is idle, and
// returns the instance for later calls unless the call failed.
type Module struct {
	// Digest is the lowercase hexadecimal SHA-256 digest of the module.
	Digest   string
	runtime  *Runtime
	compiled wazero.CompiledModule
	// slots holds a token for each instance that exists or is being made.
	slots  chan struct{}
	mu     sync.Mutex
	idle   []*instance
	closed bool
}

// An instance is one instantiation of a module, serving one call at a time.
type instance struct {
	module api.Module
	// out is the output of the call the instance serves, or nil between
	// calls.
	out *output
}

// Load compiles a module and checks that it is a provider plugin built for
// this ABI version.
func (r *Runtime) Load(ctx context.Context, module []byte) (*Module, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, errClosedRuntime
	}
	sum := sha256.Sum256(module)
	declared, err := declare(module)
	if err != nil {
		return nil, err
	}
	// Every frame counts towards its call's stack limit.
	compiled, err := r.engine.CompileModule(experimental.WithFunctionListenerFactory(ctx, frames(declared)), module)
	if err != nil {
		return nil, errNotModule
	}
	m := &Module{Digest: hex.EncodeToString(sum[:]), runtime: r, compiled: compiled, slots: make(chan struct{}, r.limits.Instances)}
	if err = m.checkABI(ctx); err != nil {
		compiled.Close(ctx)
		return nil, err
	}
	return m, nil
}

// Close releases the compiled module and its instances. A call still running
// finishes, and its instance is closed afterwards.
func (m *Module) Close(ctx context.Context) error {
	m.mu.Lock()
	idle := m.idle
	m.idle, m.closed = nil, true
	m.mu.Unlock()
	for _, instance := range idle {
		instance.module.Close(ctx)
	}
	return m.compiled.Close(ctx)
}

// Inspect loads a module and returns the manifest it declares. It refuses a
// module that is not a plugin, was built for another ABI version, declares an
// invalid manifest or names a dialect OLP doesn't have.
func (r *Runtime) Inspect(ctx context.Context, module []byte) (abi.Manifest, error) {
	m, err := r.Load(ctx, module)
	if err != nil {
		return abi.Manifest{}, err
	}
	defer m.Close(context.WithoutCancel(ctx))
	return inspect(ctx, m, false)
}

// inspect reads the manifest a plugin's code declares, which is unconfined or
// not, and validates it.
func inspect(ctx context.Context, plugin code, unconfined bool) (abi.Manifest, error) {
	var raw json.RawMessage
	if err := plugin.Call(ctx, Call{Method: abi.MethodManifest}, &raw); err != nil {
		if reported, ok := errors.AsType[*abi.Error](err); ok {
			return abi.Manifest{}, refuse(CodeManifestInvalid, "The plugin reported no manifest: "+reported.Message)
		}
		return abi.Manifest{}, err
	}
	return decodeManifest(raw, unconfined)
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
	return m.run(ctx, Call{Method: abi.ExportVersion}, func(ctx context.Context, instance api.Module) error {
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
	// HTTP, when set, grants the call the http capability.
	HTTP *HTTP
}

// Call serves call on an instance of the module and decodes its result into
// result. A call that exceeds its limits, traps or exits fails with an *Error,
// and its instance is discarded, so it leaves nothing behind; a failure the
// plugin reports is an *abi.Error.
func (m *Module) Call(ctx context.Context, call Call, result any) error {
	var request []byte
	message, err := call.request()
	if err == nil {
		request, err = json.Marshal(message)
	}
	if err != nil {
		return err
	}
	return m.run(ctx, call, func(ctx context.Context, instance api.Module) error {
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
		return decodeResult(callOutput(ctx), call.Method, response, result)
	})
}

// context returns ctx granting the plugin code that serves the call its
// capabilities, whatever runs the code: logging to out, and whatever else the
// call grants.
func (c Call) context(ctx context.Context, out *output) context.Context {
	ctx = context.WithValue(ctx, outputKey{}, out)
	return context.WithValue(ctx, httpKey{}, c.HTTP)
}

// request is the request the plugin serves for the call.
func (c Call) request() (abi.Request, error) {
	params, err := json.Marshal(c.Params)
	return abi.Request{Method: c.Method, Params: params, Provider: c.Provider}, err
}

// decodeResult decodes the plugin's response to a call of method into
// result, or returns the failure the plugin reported, redacted by out.
func decodeResult(out *output, method string, response abi.Response, result any) error {
	if err := reportedFailure(out, response); err != nil {
		return err
	}
	if result == nil {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(response.Result))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(result); err != nil {
		return refuse(CodeFailed, "The plugin's result does not match the "+method+" result.")
	}
	return nil
}

// reportedFailure returns the failure the plugin's response reports, redacted
// by out, or nil when it reports none.
func reportedFailure(out *output, response abi.Response) error {
	if response.Error == nil {
		return nil
	}
	return &abi.Error{Code: out.redact(response.Error.Code), Message: out.redact(response.Error.Message)}
}

// run serves one call on an instance within the runtime's limits.
func (m *Module) run(ctx context.Context, call Call, use func(context.Context, api.Module) error) error {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, m.runtime.limits.Time)
	defer cancel()
	out := newOutput(m.runtime.log.With("plugin_digest", m.Digest, "plugin_method", call.Method), call.Secrets)
	defer out.close()
	ctx = call.context(ctx, out)
	ctx = context.WithValue(ctx, stackKey{}, &stack{limit: uint64(m.runtime.limits.Stack) / 8})
	instance, err := m.acquire(ctx, out)
	if err == nil {
		err = use(ctx, instance.module)
		// A failure the plugin reported leaves its instance as sound as a
		// success does; after any other, the instance may be in any state.
		_, reported := errors.AsType[*abi.Error](err)
		m.release(instance, err == nil || reported)
	}
	if err == nil || isReported(err) {
		return err
	}
	if parent.Err() != nil {
		return parent.Err()
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return refuse(CodeTimedOut, fmt.Sprintf("The plugin exceeded its %s time limit.", m.runtime.limits.Time))
	case errors.Is(err, errStackExhausted):
		return refuse(CodeFailed, fmt.Sprintf("The plugin exhausted its %d KiB stack limit.", m.runtime.limits.Stack>>10))
	}
	return refuse(CodeFailed, "The plugin stopped: it trapped, exited or exhausted its memory limit.")
}

// acquire takes an idle instance for a call whose output is out, or
// instantiates the module when none is idle and the pool has room.
func (m *Module) acquire(ctx context.Context, out *output) (*instance, error) {
	select {
	case m.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	m.mu.Lock()
	var taken *instance
	if n := len(m.idle); n > 0 {
		taken, m.idle = m.idle[n-1], m.idle[:n-1]
	}
	m.mu.Unlock()
	if taken != nil {
		taken.out = out
		return taken, nil
	}
	made := &instance{out: out}
	config := wazero.NewModuleConfig().
		WithName("").
		WithStartFunctions("_initialize").
		WithSysWalltime().
		WithSysNanotime().
		WithRandSource(rand.Reader).
		WithStdout(console{made, "stdout"}).
		WithStderr(console{made, "stderr"})
	module, err := m.runtime.engine.InstantiateModule(ctx, m.compiled, config)
	if err != nil {
		<-m.slots
		return nil, err
	}
	made.module = module
	return made, nil
}

// release returns an instance to the pool after a call, or closes it.
func (m *Module) release(taken *instance, reusable bool) {
	taken.out = nil
	m.mu.Lock()
	if reusable && !m.closed {
		m.idle = append(m.idle, taken)
		taken = nil
	}
	m.mu.Unlock()
	if taken != nil {
		taken.module.Close(context.Background())
	}
	<-m.slots
}

// console carries one of an instance's output streams to the call it serves.
type console struct {
	instance *instance
	name     string
}

func (c console) Write(p []byte) (int, error) {
	if out := c.instance.out; out != nil {
		return out.stream(c.name).Write(p)
	}
	return len(p), nil
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
	default:
		response = capability(ctx, request)
	}
	data, _ := json.Marshal(response)
	ptr, err := lend(ctx, instance, data)
	if err != nil {
		panic(err)
	}
	stack[0] = abi.Pack(ptr, uint32(len(data)))
}

// capability serves one capability request from plugin code serving the call
// ctx belongs to, whatever runs the code.
func capability(ctx context.Context, request abi.Request) abi.Response {
	var response abi.Response
	switch request.Method {
	case abi.CapabilityLog:
		var record abi.LogRecord
		if json.Unmarshal(request.Params, &record) != nil {
			response.Error = &abi.Error{Code: abi.CodeInvalidRequest, Message: "A log request carries a log record."}
			break
		}
		callOutput(ctx).record(record)
	case abi.CapabilityHTTP:
		response.Result, response.Error = serveHTTP(ctx, request.Params)
	default:
		response.Error = &abi.Error{Code: abi.CodeUnknownMethod, Message: "OLP grants this call no capability named " + request.Method + "."}
	}
	return response
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
