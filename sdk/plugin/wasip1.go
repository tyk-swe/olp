//go:build wasip1

package plugin

import (
	"runtime"
	"unsafe"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// lent holds the buffers olp_alloc handed to OLP until OLP passes them back,
// so the garbage collector keeps them alive meanwhile.
var lent = map[uint32][]byte{}

// answer keeps the last olp_call response alive while OLP reads it.
var answer []byte

//go:wasmexport olp_abi_version
func abiVersion() int32 { return abi.Version }

//go:wasmexport olp_alloc
func alloc(size uint32) uint32 {
	buffer := make([]byte, max(size, 1))
	ptr := address(buffer)
	lent[ptr] = buffer
	return ptr
}

//go:wasmexport olp_call
func call(ptr, size uint32) uint64 {
	answer = serve(reclaim(ptr, size))
	return abi.Pack(address(answer), uint32(len(answer)))
}

//go:wasmimport olp host_call
func importedHostCall(ptr, size uint32) uint64

func hostCall(request []byte) []byte {
	packed := importedHostCall(address(request), uint32(len(request)))
	runtime.KeepAlive(request)
	return reclaim(abi.Unpack(packed))
}

// reclaim takes back a buffer olp_alloc lent to OLP.
func reclaim(ptr, size uint32) []byte {
	buffer, ok := lent[ptr]
	delete(lent, ptr)
	if !ok || int(size) > len(buffer) {
		panic("plugin: OLP passed a buffer the plugin did not lend")
	}
	return buffer[:size]
}

func address(buffer []byte) uint32 {
	return uint32(uintptr(unsafe.Pointer(unsafe.SliceData(buffer))))
}
