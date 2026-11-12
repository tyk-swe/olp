// Package gateway is the execution kernel behind every inference surface: it
// owns the call, route selection, the attempt loop, streaming commitment and
// settlement on top of a pinned runtime release. The packages beneath it hold
// what the kernel uses and never import it back: fault, events, quota, ingress
// and health are its building blocks, while codeforward and traceingest are
// handlers that need no call.
package gateway
