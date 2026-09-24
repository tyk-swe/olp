//go:build integration && pythonsdk

package integration_test

import "testing"

func TestNegotiatedContinuationOfficialPythonSDK(t *testing.T) {
	testContinuationSDK(t, "uv", "run", "--project", "tests/sdk-smoke-python", "--frozen", "python", "tests/sdk-smoke-python/negotiated_continuation.py")
}
