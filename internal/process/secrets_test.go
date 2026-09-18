package process

import (
	"context"
	"github.com/tyk-swe/olp/internal/config"
	"testing"
)

func TestWorkerRequiresMasterKeyForRetainedMedia(t *testing.T) {
	_, _, _, err := loadSecrets(context.Background(), nil, config.Config{
		Mode: config.Worker, AuthHMACKeyFile: "configured", ConnectorConfigFile: "mounted.json",
	}, "installation")
	if err == nil {
		t.Fatal("worker accepted mounted connectors without the key for retained jobs")
	}
}
