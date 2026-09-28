package plugins

import (
	"context"
	"errors"
	"fmt"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

// Bounds on what a module may declare. wazero allocates declared vectors,
// byte strings, tables and locals before validating their contents, so OLP
// reads their lengths and checks their framing before compilation.
// A Go plugin declares one table of a few thousand elements and functions of
// fewer than twenty locals.
const (
	// maxTableElements bounds the elements of a module's one table.
	maxTableElements = 1 << 16
	// maxFunctionLocals bounds the locals of one function.
	maxFunctionLocals = 1 << 12
	// maxModuleLocals bounds the locals of all of a module's functions.
	maxModuleLocals = 1 << 22
	// maxFunctions bounds the functions a module declares, before its
	// function and code sections allocate for them.
	maxFunctions = 1 << 16
	// maxTypes bounds a module's function types the same way, before its
	// type section allocates for them.
	maxTypes = 1 << 16
	// maxVectorEntries bounds other decoded vectors, including nested
	// element and name vectors.
	maxVectorEntries = 1 << 16
	// maxModuleEntries bounds the entries of all of a module's decoded
	// vectors together, whichever limit bounds each.
	maxModuleEntries = 1 << 20
	// frameOverhead is what a frame holds beyond its function's values, such
	// as its return address, in stack values.
	frameOverhead = 4
)

// declarations are what OLP bounds of a module before wazero compiles it.
type declarations struct {
	// frames holds, for each function the module defines, the most stack
	// values one of its frames can hold: its parameters, locals and operands.
	// An instruction pushes at most one operand per byte of its function's
	// body, or a call pushes its callee's results.
	frames []uint64
	// imported counts the functions the module imports, which precede the
	// functions it defines in its function index space.
	imported uint32
}

// Section IDs of the binary format.
const (
	sectionCustom    = 0
	sectionType      = 1
	sectionImport    = 2
	sectionFunction  = 3
	sectionTable     = 4
	sectionMemory    = 5
	sectionGlobal    = 6
	sectionExport    = 7
	sectionStart     = 8
	sectionElement   = 9
	sectionCode      = 10
	sectionData      = 11
	sectionDataCount = 12
)

// wasmHeader is the magic number and version every WebAssembly module begins
// with.
const wasmHeader = "\x00asm\x01\x00\x00\x00"

// declare bounds every allocation-driving declaration, including nested
// vectors and strings, before wazero decodes it. Semantic validation and
// instruction validation remain wazero's responsibility.
func declare(module []byte) (declarations, error) {
	if len(module) > maxModuleBytes {
		return declarations{}, errNotModule
	}
	budget := uint64(maxModuleEntries)
	r := reader{data: module, budget: &budget}
	if header := r.bytes(8); r.err != nil || string(header) != wasmHeader {
		return declarations{}, errNotModule
	}
	var d declarations
	var params, results []uint32
	var types []uint32
	var codes []function
	var seen uint16
	for r.len() > 0 && r.err == nil {
		id := r.byte()
		section := reader{data: r.bytes(r.u32()), budget: &budget}
		if r.err != nil {
			break
		}
		if id > sectionDataCount || (id != sectionCustom && seen&(1<<id) != 0) {
			return declarations{}, errNotModule
		}
		seen |= 1 << id
		switch id {
		case sectionCustom:
			section.custom()
		case sectionType:
			params, results = section.types()
		case sectionImport:
			imported, err := section.imports()
			if err != nil {
				return declarations{}, err
			}
			d.imported = imported
		case sectionFunction:
			types = section.functions()
		case sectionTable:
			if err := section.tables(); err != nil {
				return declarations{}, err
			}
		case sectionMemory:
			section.memory()
		case sectionGlobal:
			section.globals()
		case sectionExport:
			section.exports()
		case sectionStart, sectionDataCount:
			section.u32()
		case sectionElement:
			section.elements()
		case sectionCode:
			var err error
			if codes, err = section.codes(); err != nil {
				return declarations{}, err
			}
		case sectionData:
			section.segments()
		}
		if section.len() != 0 {
			section.fail()
		}
		if section.err != nil {
			r.err = section.err
		}
	}
	if r.err != nil || len(types) != len(codes) {
		return declarations{}, errNotModule
	}
	operands := uint64(1)
	for _, n := range results {
		operands = max(operands, uint64(n))
	}
	d.frames = make([]uint64, len(codes))
	for i, c := range codes {
		if types[i] >= uint32(len(params)) {
			return declarations{}, errNotModule
		}
		d.frames[i] = frameOverhead + uint64(params[types[i]]) + c.locals + c.body*operands
	}
	return d, nil
}

// errNotModule refuses bytes that are not a WebAssembly module wazero could
// compile.
var errNotModule = refuse(CodeModuleInvalid, "The upload is not a WebAssembly module OLP can run.")

// function is what OLP bounds of one function's code.
type function struct {
	// locals counts its locals, and body the bytes of its instructions.
	locals, body uint64
}

// reader reads the binary format. After a malformed read, err is set and
// every later read returns zero values.
type reader struct {
	data   []byte
	err    error
	budget *uint64
}

func (r *reader) len() int { return len(r.data) }

func (r *reader) fail() { r.err, r.data = errNotModule, nil }

func (r *reader) byte() byte {
	if len(r.data) == 0 {
		r.fail()
		return 0
	}
	b := r.data[0]
	r.data = r.data[1:]
	return b
}

func (r *reader) bytes(n uint32) []byte {
	if uint64(n) > uint64(len(r.data)) {
		r.fail()
		return nil
	}
	b := r.data[:n]
	r.data = r.data[n:]
	return b
}

// u32 reads an unsigned LEB128 integer of at most 32 bits.
func (r *reader) u32() uint32 {
	var v uint64
	for shift := 0; shift < 35; shift += 7 {
		b := r.byte()
		v |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			if v > 1<<32-1 {
				break
			}
			return uint32(v)
		}
	}
	r.fail()
	return 0
}

// vector bounds a decoded vector before either OLP or wazero allocates it.
// Every entry needs at least one byte, even before semantic validation.
func (r *reader) vector(limit uint32) uint32 {
	n := r.u32()
	if r.err != nil || n > limit || uint64(n) > uint64(r.len()) || uint64(n) > *r.budget {
		r.fail()
		return 0
	}
	*r.budget -= uint64(n)
	return n
}

// valueType reads the single-byte value types in the enabled core features.
// Typed references and GC types are not enabled and must not misalign the
// following allocation-driving lengths.
func (r *reader) valueType() {
	switch r.byte() {
	case 0x7f, 0x7e, 0x7d, 0x7c, 0x7b, 0x70, 0x6f:
	default:
		r.fail()
	}
}

// integer skips a signed constant's LEB128 immediate; wazero validates its
// value and sign bits. Unsigned lengths and indices always use u32 instead.
func (r *reader) integer(bytes int) {
	for range bytes {
		if r.byte()&0x80 == 0 {
			return
		}
	}
	r.fail()
}

func (r *reader) expression() {
	for r.err == nil {
		switch r.byte() {
		case 0x0b: // end
			return
		case 0x41: // i32.const
			r.integer(5)
		case 0x42: // i64.const
			r.integer(10)
		case 0x43: // f32.const
			r.bytes(4)
		case 0x44: // f64.const
			r.bytes(8)
		case 0x23, 0xd2: // global.get, ref.func
			r.u32()
		case 0xd0: // ref.null
			r.valueType()
		case 0xfd: // v128.const
			if r.byte() != 0x0c {
				r.fail()
			}
			r.bytes(16)
		default:
			r.fail()
		}
	}
}

// types reads the type section: each function type's parameter and result
// counts.
func (r *reader) types() (params, results []uint32) {
	n := r.vector(maxTypes)
	for i := uint32(0); i < n && r.err == nil; i++ {
		if r.byte() != 0x60 {
			r.fail()
			break
		}
		p := r.vector(maxFunctionLocals)
		for j := uint32(0); j < p && r.err == nil; j++ {
			r.valueType()
		}
		q := r.vector(maxFunctionLocals)
		for j := uint32(0); j < q && r.err == nil; j++ {
			r.valueType()
		}
		params, results = append(params, p), append(results, q)
	}
	return params, results
}

// imports reads the import section and counts its function imports. OLP
// provides only functions, so a module importing anything else is refused.
func (r *reader) imports() (uint32, error) {
	var functions uint32
	n := r.vector(maxFunctions)
	for i := uint32(0); i < n && r.err == nil; i++ {
		module := r.bytes(r.u32())
		name := r.bytes(r.u32())
		if kind := r.byte(); kind != 0x00 {
			if r.err != nil {
				break
			}
			return 0, refuse(CodeModuleInvalid, fmt.Sprintf("The module imports %s.%s, which is not a function. OLP provides plugins only functions.", module, name))
		}
		r.u32()
		functions++
	}
	return functions, nil
}

// functions reads the function section: the type of each function the
// module defines.
func (r *reader) functions() []uint32 {
	n := r.vector(maxFunctions)
	var types []uint32
	for i := uint32(0); i < n && r.err == nil; i++ {
		types = append(types, r.u32())
	}
	return types
}

// tables reads the table section and refuses more than one table, or one
// whose minimum size exceeds maxTableElements. Without reference types, a
// table never grows.
func (r *reader) tables() error {
	n := r.u32()
	if n > 1 {
		return refuse(CodeModuleInvalid, "The module declares more than one table. A plugin may declare one.")
	}
	if n == 0 {
		return nil
	}
	funcref, limits := r.byte(), r.byte()
	if funcref != 0x70 || limits > 0x01 {
		r.fail()
		return nil
	}
	if size := r.u32(); size > maxTableElements {
		return refuse(CodeModuleInvalid, fmt.Sprintf("The module declares a table of %d elements. A plugin's table may hold at most %d.", size, maxTableElements))
	}
	if limits == 0x01 {
		r.u32()
	}
	return nil
}

func (r *reader) memory() {
	n := r.vector(1)
	if n == 0 {
		return
	}
	flags := r.byte()
	if flags > 1 { // Shared and 64-bit memory are not enabled.
		r.fail()
		return
	}
	r.u32()
	if flags == 1 {
		r.u32()
	}
}

func (r *reader) globals() {
	n := r.vector(maxVectorEntries)
	for i := uint32(0); i < n && r.err == nil; i++ {
		r.valueType()
		r.byte() // mutability
		r.expression()
	}
}

func (r *reader) exports() {
	n := r.vector(maxVectorEntries)
	for i := uint32(0); i < n && r.err == nil; i++ {
		r.bytes(r.u32()) // name
		r.byte()         // kind
		r.u32()          // index
	}
}

func (r *reader) elements() {
	n := r.vector(maxVectorEntries)
	for i := uint32(0); i < n && r.err == nil; i++ {
		flags := r.u32()
		if flags > 7 {
			r.fail()
			return
		}
		if flags&1 == 0 { // active segment
			if flags&2 != 0 {
				r.u32() // table index
			}
			r.expression()
		}
		if flags != 0 && flags != 4 {
			if flags&4 == 0 {
				if r.byte() != 0 { // elemkind: function indices
					r.fail()
				}
			} else {
				r.valueType()
			}
		}
		entries := r.vector(maxVectorEntries)
		for j := uint32(0); j < entries && r.err == nil; j++ {
			if flags&4 == 0 {
				r.u32()
			} else {
				r.expression()
			}
		}
	}
}

func (r *reader) segments() {
	n := r.vector(maxVectorEntries)
	for i := uint32(0); i < n && r.err == nil; i++ {
		switch r.u32() {
		case 0:
			r.expression()
		case 1: // passive segment
		case 2:
			r.u32() // memory index
			r.expression()
		default:
			r.fail()
		}
		r.bytes(r.u32())
	}
}

func (r *reader) custom() {
	name := r.bytes(r.u32())
	if string(name) != "name" {
		// Other custom payloads are opaque; debug information is disabled.
		r.bytes(uint32(r.len()))
		return
	}
	for r.len() > 0 && r.err == nil {
		id := r.byte()
		sub := reader{data: r.bytes(r.u32()), budget: r.budget}
		switch id {
		case 0: // module name
			sub.bytes(sub.u32())
		case 1: // function names
			sub.names()
		case 2: // local names, grouped by function
			n := sub.vector(maxFunctions)
			for i := uint32(0); i < n && sub.err == nil; i++ {
				sub.u32()
				sub.names()
			}
		default: // wazero skips other name subsections.
			sub.bytes(uint32(sub.len()))
		}
		if sub.err != nil || sub.len() != 0 {
			r.fail()
		}
	}
}

func (r *reader) names() {
	n := r.vector(maxVectorEntries)
	for i := uint32(0); i < n && r.err == nil; i++ {
		r.u32()          // index
		r.bytes(r.u32()) // name
	}
}

// codes reads the code section and refuses more than maxFunctions functions,
// a function whose locals exceed maxFunctionLocals, or locals in all that
// exceed maxModuleLocals.
func (r *reader) codes() ([]function, error) {
	n := r.vector(maxFunctions)
	var codes []function
	var total uint64
	for i := uint32(0); i < n && r.err == nil; i++ {
		body := reader{data: r.bytes(r.u32()), budget: r.budget}
		var c function
		declared := body.vector(maxVectorEntries)
		for j := uint32(0); j < declared && body.err == nil; j++ {
			c.locals += uint64(body.u32())
			body.valueType()
			if c.locals > maxFunctionLocals {
				return nil, refuse(CodeModuleInvalid, fmt.Sprintf("The module declares a function of more than %d locals.", maxFunctionLocals))
			}
		}
		if total += c.locals; total > maxModuleLocals {
			return nil, refuse(CodeModuleInvalid, fmt.Sprintf("The module declares more than %d locals in all.", maxModuleLocals))
		}
		if body.err != nil {
			r.err = body.err
			break
		}
		c.body = uint64(body.len())
		codes = append(codes, c)
	}
	return codes, nil
}

// stack is how many stack values the frames of a call's plugin code hold.
type stack struct {
	used, limit uint64
}

type stackKey struct{}

// errStackExhausted fails a call whose frames exceed its stack limit.
var errStackExhausted = errors.New("stack limit exhausted")

// frames bounds the stack of a module's calls: each frame a function enters
// counts the most values it can hold towards its call's stack limit.
type frames declarations

func (f frames) NewFunctionListener(definition api.FunctionDefinition) experimental.FunctionListener {
	if _, _, imported := definition.Import(); imported {
		return nil
	}
	return frame(f.frames[definition.Index()-f.imported])
}

// frame is the most stack values a function's frame holds.
type frame uint64

func (f frame) Before(ctx context.Context, _ api.Module, _ api.FunctionDefinition, _ []uint64, _ experimental.StackIterator) {
	s, _ := ctx.Value(stackKey{}).(*stack)
	if s == nil {
		return
	}
	if s.used += uint64(f); s.used > s.limit {
		panic(errStackExhausted)
	}
}

func (f frame) After(ctx context.Context, _ api.Module, _ api.FunctionDefinition, _ []uint64) {
	if s, _ := ctx.Value(stackKey{}).(*stack); s != nil {
		s.used -= uint64(f)
	}
}

func (frame) Abort(context.Context, api.Module, api.FunctionDefinition, error) {}
