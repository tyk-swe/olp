package plugins

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// pluginTable is an olp.plugins table holding at most one approved plugin,
// which it serves to Usable whatever digest is asked for, counting the reads.
type pluginTable struct {
	mu     sync.Mutex
	module []byte
	reads  int
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

func (p *pluginTable) QueryRow(context.Context, string, ...any) pgx.Row {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reads++
	module := p.module
	return scanner(func(dest ...any) error {
		if module == nil {
			return pgx.ErrNoRows
		}
		*dest[0].(*[]byte), *dest[1].(*bool), *dest[2].(*[]byte) = []byte(`{}`), true, module
		return nil
	})
}

func (p *pluginTable) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("the plugin table serves single rows")
}

type scanner func(dest ...any) error

func (s scanner) Scan(dest ...any) error { return s(dest...) }

func newTestHost(t testing.TB, engine Engine, limits Limits, log *slog.Logger, module []byte) (*Host, *pluginTable) {
	t.Helper()
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	r, err := NewRuntime(t.Context(), engine, limits, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(context.Background()) })
	table := &pluginTable{module: module}
	return NewHost(r, table), table
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

// The reference plugin signs a request with an HMAC of its static credential
// and a timestamp, which an upstream holding the credential can verify.
func TestReferencePluginSignsWithAnHMACOfItsCredential(t *testing.T) {
	t.Parallel()
	host, _ := newTestHost(t, Interpreted, DefaultLimits, nil, testutil.BuildPlugin(t, "./sdk/plugin/reference"))
	body := []byte(`{"model":"reference","messages":[{"role":"user","content":"hi"}]}`)
	request := abi.SignRequest{Profile: "reference-signed-chat", Method: "POST", URL: "https://api.example.com/v1/chat/completions", Body: body, Credential: "sk-reference"}
	result, err := host.Sign(t.Context(), "reference", referenceProvider, request, []string{"sk-reference"})
	if err != nil {
		t.Fatal(err)
	}
	timestamp := result.Headers["X-Reference-Timestamp"]
	if seconds, err := strconv.ParseInt(timestamp, 10, 64); err != nil || time.Since(time.Unix(seconds, 0)).Abs() > time.Minute {
		t.Fatalf("timestamp %q", timestamp)
	}
	mac := hmac.New(sha256.New, []byte("sk-reference"))
	mac.Write([]byte(timestamp + "\nPOST\n/v1/chat/completions\n"))
	mac.Write(body)
	if len(result.Headers) != 2 || result.Headers["X-Reference-Signature"] != hex.EncodeToString(mac.Sum(nil)) {
		t.Fatalf("signed headers %v", result.Headers)
	}
}

// A host compiles a plugin's module once and serves later calls from the
// instances it keeps, so no request compiles or instantiates anything.
func TestHostCompilesAPluginOnceAndReusesItsInstances(t *testing.T) {
	t.Parallel()
	host, table := newTestHost(t, Interpreted, DefaultLimits, nil, fixture(t, "well-behaved"))
	for want := 1; want <= 3; want++ {
		if calls := signedCalls(t, host); calls != strconv.Itoa(want) {
			t.Fatalf("call %d ran on an instance that signed %s requests", want, calls)
		}
	}
	if reads := table.read(); reads != 1 {
		t.Fatalf("the host read the module %d times", reads)
	}
}

// Concurrent calls share a pool of at most Limits.Instances instances.
func TestHostBoundsTheInstancesOfAModule(t *testing.T) {
	t.Parallel()
	limits := DefaultLimits
	limits.Instances = 1
	host, _ := newTestHost(t, Interpreted, limits, nil, fixture(t, "well-behaved"))
	var mu sync.Mutex
	var counts []string
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			calls := signedCalls(t, host)
			mu.Lock()
			counts = append(counts, calls)
			mu.Unlock()
		})
	}
	wg.Wait()
	slices.Sort(counts)
	if strings.Join(counts, ",") != "1,2,3,4,5,6" {
		t.Fatalf("one instance did not serve every call: %v", counts)
	}
}

// A signing hook receives the provider it signs for, with the provider's
// option values, as its call's provider.
func TestSigningHookSeesTheProviderItSignsFor(t *testing.T) {
	t.Parallel()
	host, _ := newTestHost(t, Interpreted, DefaultLimits, nil, fixture(t, "well-behaved"))
	provider := abi.Provider{Profile: "fixture-chat", Options: map[string]string{"workspace": "acme"}}
	result, err := host.Sign(t.Context(), fixtureDigest, provider, signRequest("option:workspace", nil), nil)
	if err != nil || result.Headers["X-Fixture-Option"] != "fixture-chat acme" {
		t.Fatalf("signed %v: %v", result.Headers, err)
	}
}

// A signing hook past its limits fails with a typed error, and its instance is
// discarded, so the next call starts on a fresh one. A failure the plugin
// reports leaves its instance serving.
func TestSigningFailuresLeaveNothingBehind(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		credential, code string
		limits           Limits
		fresh            bool
	}{
		// Time enough to instantiate the module afresh under the race
		// detector; generous time elsewhere, so the memory limit is what
		// stops the call.
		"time":     {"loop", CodeTimedOut, Limits{Memory: 32 << 20, Time: 5 * time.Second, Instances: 1}, true},
		"memory":   {"allocate", CodeFailed, Limits{Memory: 16 << 20, Time: time.Minute, Instances: 1}, true},
		"reported": {"fail:" + fixtureSecret, "fixture_failed", Limits{Memory: 16 << 20, Time: time.Minute, Instances: 1}, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			host, _ := newTestHost(t, Interpreted, tc.limits, nil, fixture(t, "well-behaved"))
			_, err := host.Sign(t.Context(), fixtureDigest, fixtureProvider, signRequest(tc.credential, nil), []string{tc.credential})
			if reported, ok := errors.AsType[*abi.Error](err); ok {
				if reported.Code != tc.code || reported.Message != "signing with [REDACTED] failed" {
					t.Fatalf("the plugin reported %+v", reported)
				}
			} else {
				wantError(t, err, tc.code, "")
			}
			want := "2"
			if tc.fresh {
				want = "1"
			}
			if calls := signedCalls(t, host); calls != want {
				t.Fatalf("the next call ran on an instance that signed %s requests", calls)
			}
		})
	}
}

// A plugin the host could not load is not remembered: once it is usable, the
// next call loads it.
func TestHostLoadsAPluginOnceItIsUsable(t *testing.T) {
	t.Parallel()
	host, table := newTestHost(t, Interpreted, DefaultLimits, nil, nil)
	_, err := host.Sign(t.Context(), fixtureDigest, fixtureProvider, signRequest("sk-fixture", nil), nil)
	wantError(t, err, CodeNotInstalled, "")
	table.install(fixture(t, "well-behaved"))
	if calls := signedCalls(t, host); calls != "1" {
		t.Fatalf("the installed plugin signed %s requests", calls)
	}
}

func TestSigningHookOutputReachesTheLogRedacted(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	host, _ := newTestHost(t, Interpreted, DefaultLimits, slog.New(slog.NewJSONHandler(&logged, nil)), fixture(t, "well-behaved"))
	credential := "log:" + fixtureSecret
	if _, err := host.Sign(t.Context(), fixtureDigest, fixtureProvider, signRequest(credential, nil), []string{credential}); err != nil {
		t.Fatal(err)
	}
	output := logged.String()
	if strings.Contains(output, fixtureSecret) || !strings.Contains(output, `"msg":"signing with [REDACTED]"`) || !strings.Contains(output, `"plugin_method":"sign"`) {
		t.Fatalf("signing hook output %s", output)
	}
}

func TestHostEvictsTheLeastRecentlyUsedIdleCode(t *testing.T) {
	host := &Host{hosted: map[string]*hosted{}}
	for i := range maxHosted {
		host.hosted[strconv.Itoa(i)] = &hosted{code: &Module{Digest: strconv.Itoa(i)}, used: uint64(i + 1)}
	}
	host.hosted["0"].calls = 1
	host.hosted["loading"] = &hosted{}
	evicted := host.evict()
	if len(evicted) != 1 || evicted[0].(*Module).Digest != "1" || len(host.hosted) != maxHosted || host.hosted["0"] == nil {
		t.Fatalf("evicted %v, kept %d", evicted, len(host.hosted))
	}
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
