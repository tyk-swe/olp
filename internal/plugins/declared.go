package plugins

import (
	"context"
	"errors"
	"fmt"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

// Bounds on what a module may declare. wazero allocates a table's elements
// and a function's locals as the module declares them, before any limit
// applies, so OLP reads these declarations before wazero compiles a module.
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
	sectionType     = 1
	sectionImport   = 2
	sectionFunction = 3
	sectionTable    = 4
	sectionCode     = 10
)

// declare reads what a module declares and refuses a module whose tables or
// locals exceed OLP's bounds, or that imports anything but functions. It
// reads only the sections it bounds, and leaves validating the module to
// wazero.
func declare(module []byte) (declarations, error) {
	r := reader{data: module}
	if header := r.bytes(8); r.err != nil || string(header) != "\x00asm\x01\x00\x00\x00" {
		return declarations{}, errNotModule
	}
	var d declarations
	var params, results []uint32
	var types []uint32
	var codes []function
	for r.len() > 0 && r.err == nil {
		id := r.byte()
		section := reader{data: r.bytes(r.u32())}
		if r.err != nil {
			break
		}
		switch id {
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
		case sectionCode:
			var err error
			if codes, err = section.codes(); err != nil {
				return declarations{}, err
			}
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
	data []byte
	err  error
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

// types reads the type section: each function type's parameter and result
// counts.
func (r *reader) types() (params, results []uint32) {
	n := r.u32()
	if n > maxTypes {
		r.err = refuse(CodeModuleInvalid, fmt.Sprintf("The module declares %d function types. A plugin may declare at most %d.", n, maxTypes))
		return nil, nil
	}
	for i := uint32(0); i < n && r.err == nil; i++ {
		if r.byte() != 0x60 {
			r.fail()
			break
		}
		p := r.u32()
		r.bytes(p)
		q := r.u32()
		r.bytes(q)
		params, results = append(params, p), append(results, q)
	}
	return params, results
}

// imports reads the import section and counts its function imports. OLP
// provides only functions, so a module importing anything else is refused.
func (r *reader) imports() (uint32, error) {
	var functions uint32
	n := r.u32()
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
	n := r.u32()
	if n > maxFunctions {
		r.err = refuse(CodeModuleInvalid, fmt.Sprintf("The module declares %d functions. A plugin may declare at most %d.", n, maxFunctions))
		return nil
	}
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

// codes reads the code section and refuses more than maxFunctions functions,
// a function whose locals exceed maxFunctionLocals, or locals in all that
// exceed maxModuleLocals.
func (r *reader) codes() ([]function, error) {
	n := r.u32()
	if n > maxFunctions {
		return nil, refuse(CodeModuleInvalid, fmt.Sprintf("The module declares %d functions. A plugin may declare at most %d.", n, maxFunctions))
	}
	var codes []function
	var total uint64
	for i := uint32(0); i < n && r.err == nil; i++ {
		body := reader{data: r.bytes(r.u32())}
		var c function
		declared := body.u32()
		for j := uint32(0); j < declared && body.err == nil; j++ {
			c.locals += uint64(body.u32())
			body.byte()
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
