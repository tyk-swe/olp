// Package gateway serves the native OpenAI inference surface (/v1) and the
// console playground on top of a pinned runtime release: bounded ingress,
// API key authentication, route selection, attempt failover, streaming
// commitment, circuit health, and the terminal request envelope.
package gateway
