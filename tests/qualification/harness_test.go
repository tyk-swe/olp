//go:build qualification

package qualification_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/tests/harness"
)

type service struct{ *harness.Service }

func startService(t *testing.T, adjust func(*harness.Environment)) *service {
	t.Helper()
	pool, databaseURL := harness.EmptyDatabase(t)
	env := harness.NewEnvironment(t, databaseURL, "Qualification")
	if adjust != nil {
		adjust(&env)
	}
	harness.RunBinary(t, env, "migrate")
	return &service{harness.StartProcess(t, pool, env)}
}

func uniqueName(prefix string) string { return prefix + "-" + uuid.NewString()[:8] }
