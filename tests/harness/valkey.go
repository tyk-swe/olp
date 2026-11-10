package harness

import (
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/coordination"
)

func Valkey(t *testing.T) *coordination.Client {
	t.Helper()
	configuration, err := coordination.Configuration(Required(t, "OLP_TEST_VALKEY_URL"), "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client, err := coordination.Open(t.Context(), configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client
}
