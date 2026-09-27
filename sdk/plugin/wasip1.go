//go:build wasip1

package plugin

import (
	"context"
	"encoding/json"
	"runtime"
	"unsafe"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// lent holds the buffers olp_alloc handed to OLP until OLP passes them back,
// so the garbage collector keeps them alive meanwhile.
var lent = map[uint32][]byte{}

// reply keeps the last olp_call response alive while OLP reads it.
var reply []byte

// Serve does nothing in a WASI reactor, whose main function never runs: OLP
// calls the module's exports instead.
func Serve() {}

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
	reply = serve(reclaim(ptr, size))
	return abi.Pack(address(reply), uint32(len(reply)))
}

//go:wasmimport olp host_call
func importedHostCall(ptr, size uint32) uint64

// hostCall passes a capability request to OLP. A module serves one call at a
// time, so the request serves the call being served.
func hostCall(_ context.Context, request abi.Request) abi.Response {
	message, err := json.Marshal(request)
	if err != nil {
		return answer(nil, &abi.Error{Code: abi.CodeInternal, Message: "The capability request is not JSON."})
	}
	packed := importedHostCall(address(message), uint32(len(message)))
	runtime.KeepAlive(message)
	var response abi.Response
	if err = json.Unmarshal(reclaim(abi.Unpack(packed)), &response); err != nil {
		return answer(nil, &abi.Error{Code: abi.CodeInternal, Message: "OLP's response is not JSON."})
	}
	return response
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
