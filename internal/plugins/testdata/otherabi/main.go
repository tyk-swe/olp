// Command otherabi is a module built for a plugin ABI that OLP does not run.
package main

//go:wasmexport olp_abi_version
func abiVersion() int32 { return 2 }

func main() {}
