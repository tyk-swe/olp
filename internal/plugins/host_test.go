package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// pluginTable is an olp.plugins table holding at most one approved plugin,
// which it serves to Usable whatever digest is asked for, counting the reads:
// a confined plugin's module or an unconfined plugin's executable. Its
// manifest declares only its approved origins.
type pluginTable struct {
	guardrail  bool
	mu         sync.Mutex
	module     []byte
	executable string
	origins    []string
	reads      int
	// modules counts the reads that asked for a confined plugin's module.
	modules int
}

func (p *pluginTable) install(module []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.module = module
}

func (p *pluginTable) read() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reads
}

func (p *pluginTable) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reads++
	withModule := len(args) > 1 && args[1] == true
	if withModule {
		p.modules++
	}
	module, executable := p.module, p.executable
	manifest, err := json.Marshal(abi.Manifest{Origins: p.origins, Guardrail: p.guardrail})
	return scanner(func(dest ...any) error {
		switch {
		case module != nil:
			if withModule {
				*dest[3].(*[]byte) = module
			}
		case executable != "":
			*dest[2].(**string) = &executable
		default:
			return pgx.ErrNoRows
		}
		*dest[0].(*[]byte), *dest[1].(*bool) = manifest, true
		return err
	})
}

func (p *pluginTable) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("the plugin table serves single rows")
}

type scanner func(dest ...any) error

func (s scanner) Scan(dest ...any) error { return s(dest...) }

func newTestHost(t testing.TB, engine Engine, limits Limits, log *slog.Logger, module []byte) (*Host, *pluginTable) {
	t.Helper()
	table := &pluginTable{module: module}
	return NewHost(startRuntime(t, engine, limits, log), nil, table), table
}

const fixtureDigest = "fixture"

// The providers the fixture's and the reference plugin's signing profiles
// sign for.
var (
	fixtureProvider   = abi.Provider{Profile: "fixture-chat"}
	referenceProvider = abi.Provider{Profile: "reference-signed-chat"}
)

func signRequest(credential string, body []byte) abi.SignRequest {
	return abi.SignRequest{
		Profile: "fixture-chat", Method: "POST", URL: "https://api.example.com/v1/chat/completions?api-version=1",
		Header: map[string][]string{"Content-Type": {"application/json"}}, Body: body, Credential: credential,
	}
}

func signedCalls(t *testing.T, host *Host) string {
	t.Helper()
	result, err := host.Sign(t.Context(), fixtureDigest, fixtureProvider, signRequest("sk-fixture", []byte("{}")), nil)
	if err != nil {
		t.Fatal(err)
	}
	return result.Headers["X-Fixture-Calls"]
}

// A failed Fetch includes the requested URL in its error.

type heldCode struct {
	started chan struct{}
	close   func()
}

func (c *heldCode) Call(ctx context.Context, _ Call, _ any) error {
	c.started <- struct{}{}
	<-ctx.Done()
	return nil
}

func (c *heldCode) Close(context.Context) error {
	c.close()
	return nil
}

// BenchmarkSign measures the per-request overhead of a signing hook: the
// reference plugin signing chat requests on instances the host keeps.
//
//	go test ./internal/plugins -run '^$' -bench BenchmarkSign
func BenchmarkSign(b *testing.B) {
	module := testutil.BuildPlugin(b, "./sdk/plugin/reference")
	for _, engine := range []struct {
		name   string
		engine Engine
	}{{"interpreted", Interpreted}, {"compiled", Compiled}} {
		host, _ := newTestHost(b, engine.engine, DefaultLimits, nil, module)
		start := time.Now()
		if _, err := host.Sign(b.Context(), "reference", referenceProvider, abi.SignRequest{Profile: "reference-signed-chat", URL: "https://api.example.com/v1"}, nil); err != nil {
			b.Fatal(err)
		}
		b.Logf("%s: loaded and signed first in %s", engine.name, time.Since(start))
		for _, size := range []int{1 << 10, 16 << 10} {
			body := fmt.Appendf(nil, `{"model":"reference","messages":[{"role":"user","content":%q}]}`, strings.Repeat("x", size))
			request := abi.SignRequest{
				Profile: "reference-signed-chat", Method: "POST", URL: "https://api.example.com/v1/chat/completions",
				Header: map[string][]string{"Content-Type": {"application/json"}, "User-Agent": {"olp/gateway"}, "X-Reference-Client": {"olp"}},
				Body:   body, Credential: "sk-reference",
			}
			b.Run(fmt.Sprintf("%s/%dKiB", engine.name, size>>10), func(b *testing.B) {
				for b.Loop() {
					if _, err := host.Sign(b.Context(), "reference", referenceProvider, request, []string{"sk-reference"}); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// Were the call to load the plugin, it would find none installed.

// Fill the ordinary module cache. The release's pin keeps its guardrail
// available even when its LRU entry would otherwise be the first eviction.
