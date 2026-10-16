//go:build integration

package integration_test

import ()

func idemKey(name string) map[string]string { return map[string]string{"Idempotency-Key": name} }

// A replayed key returns the stored reply and creates nothing else.

// A vendor must belong to the declared kind; a plugin needs a provider.

// Settings-free members read but never create.

// Organization and project cards resolve their owners by id.

// Replay stores nothing twice.

// A provider-scoped card price is refused.
