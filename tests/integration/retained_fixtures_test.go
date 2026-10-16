//go:build integration

package integration_test

const continuationInput = `{"model":"ROUTE","stream":true,"tools":[{"type":"function","function":{"name":"weather","description":"Weather in a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}},{"type":"function","function":{"name":"clock","description":"Time in a zone","parameters":{"type":"object","properties":{"zone":{"type":"string"}},"required":["zone"]}}}],"messages":[{"role":"user","content":"Weather and time in Paris?"}]}`

// restored from tests/integration/continuation_workflow_test.go
const continuationClientVersion = "chat-anthropic-tools-v1"
