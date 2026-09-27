package plugins

import (
	"bytes"
	"runtime"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// hostile describes a module that implements plugin ABI 1, as a Go plugin
// does, except for what a hostile plugin changes: what it imports, the
// tables it declares, and the locals and instructions of its olp_call.
type hostile struct {
	imports [][]byte
	tables  [][]byte
	locals  [][]byte
	call    []byte
	// functions adds functions of no parameters, each with these locals.
	functions  int
	eachLocals uint32
}

// Types of the hostile module: olp_abi_version, olp_alloc, olp_call and the
// added functions.
var hostileTypes = [][]byte{{0x60, 0, 1, 0x7f}, {0x60, 1, 0x7f, 1, 0x7f}, {0x60, 2, 0x7f, 0x7f, 1, 0x7e}, {0x60, 0, 0}}

func (h hostile) module() []byte {
	module := []byte("\x00asm\x01\x00\x00\x00")
	module = append(module, wasmSection(sectionType, wasmVector(hostileTypes...))...)
	if h.imports != nil {
		module = append(module, wasmSection(sectionImport, wasmVector(h.imports...))...)
	}
	functions := [][]byte{{0}, {1}, {2}}
	bodies := [][]byte{
		wasmBody(nil, 0x41, abi.Version, 0x0b),      // i32.const 1
		wasmBody(nil, 0x41, 0x80, 0x08, 0x0b),       // i32.const 1024
		wasmBody(h.locals, append(h.call, 0x0b)...), // olp_call
	}
	for range h.functions {
		functions = append(functions, []byte{3})
		bodies = append(bodies, wasmBody([][]byte{append(wasmLEB(h.eachLocals), 0x7e)}, 0x0b))
	}
	module = append(module, wasmSection(sectionFunction, wasmVector(functions...))...)
	if h.tables != nil {
		module = append(module, wasmSection(sectionTable, wasmVector(h.tables...))...)
	}
	module = append(module, wasmSection(5, wasmVector([]byte{0x00, 2}))...)
	module = append(module, wasmSection(7, wasmVector(
		append(wasmName("memory"), 0x02, 0),
		append(wasmName(abi.ExportVersion), 0x00, 0),
		append(wasmName(abi.ExportAlloc), 0x00, 1),
		append(wasmName(abi.ExportCall), 0x00, 2),
	))...)
	return append(module, wasmSection(sectionCode, wasmVector(bodies...))...)
}

func wasmLEB(v uint32) []byte {
	var out []byte
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	return append(out, byte(v))
}

func wasmVector(items ...[]byte) []byte {
	return append(wasmLEB(uint32(len(items))), bytes.Join(items, nil)...)
}

func wasmName(name string) []byte { return append(wasmLEB(uint32(len(name))), name...) }

func wasmSection(id byte, payload []byte) []byte {
	return append(append([]byte{id}, wasmLEB(uint32(len(payload)))...), payload...)
}

func wasmBody(locals [][]byte, instructions ...byte) []byte {
	body := append(wasmVector(locals...), instructions...)
	return append(wasmLEB(uint32(len(body))), body...)
}

// funcrefTable declares a funcref table of min elements and no maximum.
func funcrefTable(min uint32) []byte { return append([]byte{0x70, 0x00}, wasmLEB(min)...) }

// recurse is olp_call's instructions calling itself, forever.
var recurse = []byte{0x20, 0, 0x20, 1, 0x10, 2}

// The hostile module is a plugin in every other respect: without its
// hostility, OLP loads it and calls it.
func TestHostileModuleIsOtherwiseAPlugin(t *testing.T) {
	t.Parallel()
	benign := hostile{tables: [][]byte{funcrefTable(maxTableElements)}, call: []byte{0x42, 0}, functions: 2, eachLocals: maxFunctionLocals}
	for _, engine := range []Engine{Interpreted, Compiled} {
		r := newEngineRuntime(t, engine, DefaultLimits)
		m, err := r.Load(t.Context(), benign.module())
		if err != nil {
			t.Fatal(err)
		}
		// Its olp_call returns no JSON response, which only a module OLP
		// instantiated and called can fail with.
		wantError(t, m.Call(t.Context(), Call{Method: abi.MethodManifest}, nil), CodeFailed, "")
		m.Close(t.Context())
	}
}

// wazero allocates what a module declares before any limit applies: a table's
// elements when it instantiates the module, a function's locals when it
// compiles it. OLP refuses such a module before either.
func TestInstallRefusesModulesDeclaringMoreThanOLPBounds(t *testing.T) {
	t.Parallel()
	huge := func(n int) hostile {
		return hostile{call: []byte{0x42, 0}, functions: n, eachLocals: maxFunctionLocals}
	}
	for name, tc := range map[string]struct {
		module hostile
		detail string
	}{
		// wazero would allocate the table, 1 GiB, per instance.
		"a huge table": {hostile{tables: [][]byte{funcrefTable(1 << 27)}, call: []byte{0x42, 0}}, "table of 134217728 elements"},
		"two tables":   {hostile{tables: [][]byte{funcrefTable(1), funcrefTable(1)}, call: []byte{0x42, 0}}, "more than one table"},
		// ref.null func, i32.const 0x7fffffff, table.grow 0: with reference
		// types, a table grows to what the module asks.
		"a growing table":               {hostile{tables: [][]byte{funcrefTable(1)}, call: []byte{0xd0, 0x70, 0x41, 0xff, 0xff, 0xff, 0xff, 0x07, 0xfc, 0x0f, 0x00, 0x1a, 0x42, 0}}, "not a WebAssembly module OLP can run"},
		"an imported table":             {hostile{imports: [][]byte{append(append(wasmName("olp"), wasmName("table")...), 0x01, 0x70, 0x00, 0x01)}, call: []byte{0x42, 0}}, "imports olp.table, which is not a function"},
		"a function of too many locals": {hostile{locals: [][]byte{append(wasmLEB(maxFunctionLocals+1), 0x7e)}, call: []byte{0x42, 0}}, "function of more than 4096 locals"},
		"too many locals in all":        {huge(maxModuleLocals/maxFunctionLocals + 1), "more than 4194304 locals in all"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := newTestRuntime(t, DefaultLimits, nil).Inspect(t.Context(), tc.module.module())
			wantError(t, err, CodeModuleInvalid, "")
			if !strings.Contains(err.Error(), tc.detail) {
				t.Fatalf("want a refusal naming %q, got %v", tc.detail, err)
			}
		})
	}
}

// A call's frames count towards its stack limit on either engine, however
// deep it recurses and however many operands each frame holds, so a runaway
// call fails without the stack growing far past the limit.
func TestACallPastItsStackLimitFails(t *testing.T) {
	// Not parallel: the test measures what the process allocates.
	operands := bytes.Repeat([]byte{0x42, 0}, 5000) // i64.const 0, many times
	operands = append(operands, recurse...)
	operands = append(operands, bytes.Repeat([]byte{0x1a}, 5001)...) // drop them all
	operands = append(operands, 0x42, 0)
	limits := DefaultLimits
	limits.Stack = 1 << 20
	for _, engine := range []Engine{Interpreted, Compiled} {
		for name, call := range map[string][]byte{"deep": recurse, "wide": operands} {
			r := newEngineRuntime(t, engine, limits)
			m, err := r.Load(t.Context(), hostile{call: call}.module())
			if err != nil {
				t.Fatal(err)
			}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			err = m.Call(t.Context(), Call{Method: abi.MethodManifest}, nil)
			runtime.ReadMemStats(&after)
			wantError(t, err, CodeFailed, "")
			if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 32<<20 {
				t.Fatalf("engine %d, %s: the call allocated %d MiB against a 1 MiB stack limit", engine, name, allocated>>20)
			}
			m.Close(t.Context())
		}
	}
}
