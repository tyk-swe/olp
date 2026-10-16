package plugins

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// carrierFixture is a host whose only plugin is the carrier plugin, built
// natively to carry its profile's traffic to upstream, and the plugin's
// digest.
func carrierFixture(t *testing.T, upstream string, limits Limits) (*Host, string) {
	t.Helper()
	dir := t.TempDir()
	testutil.BuildExecutablePlugin(t, filepath.Join(dir, "carrier"), "./internal/plugins/testdata/carrier", "-X=main.upstream="+upstream+"/v1")
	u := NewUnconfined(dir, limits, slog.New(slog.DiscardHandler))
	files, err := u.Executables()
	if err != nil || len(files) != 1 {
		t.Fatalf("executables %+v: %v", files, err)
	}
	return newUnconfinedHost(t, u, files[0]), files[0].Digest
}

// carriedCredential authorizes the requests the tests carry.
const carriedCredential = "sk-carried"

// carry hands the plugin with digest a chat request saying prompt to carry to
// upstream.
func carry(ctx context.Context, host *Host, digest, upstream, prompt string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"`+prompt+`"}]}`))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+carriedCredential)
	req.Header.Set("Content-Type", "application/json")
	return host.Carry(ctx, digest, abi.Provider{Profile: "carrier-chat"}, req, []string{carriedCredential})
}

// prompted reports whether an upstream request says prompt, reading its body.
func prompted(r *http.Request, prompt string) bool {
	body, _ := io.ReadAll(r.Body)
	return strings.Contains(string(body), prompt)
}

// Closing each connection makes the plugin dial every request, so the
// closed upstream refuses the last one. A pooled connection could
// otherwise carry it before the plugin noticed the upstream closing
// that connection, leaving the request's outcome unknown.

// Reading the body also lets the server notice a closed connection.

// It answers its first call's head, then reads its calls and answers none.

// The carrier reads one byte of the first request, then leaves the rest
// unread. Cancel only once the large write has actually reached stdin.
