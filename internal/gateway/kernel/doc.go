// Package kernel is the execution kernel behind every inference surface: it
// owns the call, route selection, the attempt loop, streaming commitment and
// settlement on top of a pinned runtime release. The packages beneath it hold
// what the kernel uses and never import it back: fault, events, quota, ingress
// and health are its building blocks, while codeforward, traceingest,
// delegation and models are handlers that need no call. Each package under
// surface owns the routes of one surface and calls the kernel's exported steps.
package kernel
