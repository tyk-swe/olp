package plugins

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

const fixtureSecret = "sk-fixture-secret"

func newTestRuntime(t *testing.T, limits Limits, log *slog.Logger) *Runtime {
	t.Helper()
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	r, err := NewRuntime(t.Context(), limits, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(context.Background()) })
	return r
}

func fixture(t *testing.T, behaviour string) []byte {
	return testutil.BuildPlugin(t, "./internal/plugins/testdata/fixture", "-X=main.behaviour="+behaviour)
}

func wantError(t *testing.T, err error, code, field string) {
	t.Helper()
	refusal, ok := errors.AsType[*Error](err)
	if !ok {
		t.Fatalf("want %s, got %v", code, err)
	}
	if refusal.Code != code || refusal.Field != field {
		t.Fatalf("want %s at %q, got %s at %q: %s", code, field, refusal.Code, refusal.Field, refusal.Message)
	}
}

func TestReferencePluginDeclaresItsManifest(t *testing.T) {
	t.Parallel()
	module := testutil.BuildPlugin(t, "./sdk/plugin/reference")
	r := newTestRuntime(t, DefaultLimits, nil)
	loaded, err := r.Load(t.Context(), module)
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.Close(t.Context())
	sum := sha256.Sum256(module)
	if loaded.Digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("digest %s is not the module's SHA-256", loaded.Digest)
	}
	manifest, err := r.Inspect(t.Context(), module)
	if err != nil {
		t.Fatal(err)
	}
	want := abi.Manifest{
		Name:        "reference",
		Version:     "0.1.0",
		Description: "Reference plugin for the OpenLLMProxy plugin SDK.",
		Origins:     []string{"https://api.example.com", "https://login.example.com"},
		Profiles: []abi.Profile{{ID: "reference-chat", Label: "Reference Chat Completions", Dialect: "openai-chat", Hosting: abi.Hosting{
			Address:        "https://api.example.com/v1",
			Headers:        map[string]string{"Authorization": "Token {credential}", "X-Reference-Client": "olp"},
			Discovery:      &abi.Discovery{Path: "/models", Models: "data", ID: "id", Pagination: &abi.Pagination{Parameter: "page_token", Cursor: "next_page_token"}},
			Classification: []abi.FailureRule{{Status: 400, Code: "quota_exhausted", Class: abi.ClassRateLimited}},
		}}},
	}
	if !reflect.DeepEqual(manifest, want) {
		t.Fatalf("manifest %+v, want %+v", manifest, want)
	}
}

func TestInspectRefusesModulesOLPCannotInstall(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		module      func(*testing.T) []byte
		code, field string
	}{
		"not WebAssembly": {func(*testing.T) []byte { return []byte("plugin") }, CodeModuleInvalid, ""},
		"not a plugin":    {func(*testing.T) []byte { return []byte("\x00asm\x01\x00\x00\x00") }, CodeModuleInvalid, ""},
		"another ABI version": {func(t *testing.T) []byte {
			return testutil.BuildPlugin(t, "./internal/plugins/testdata/otherabi")
		}, CodeABIUnsupported, ""},
		"invalid manifest": {func(t *testing.T) []byte { return fixture(t, "invalid-manifest") }, CodeManifestInvalid, "manifest.origins[0]"},
		"unknown dialect":  {func(t *testing.T) []byte { return fixture(t, "unknown-dialect") }, CodeDialectUnknown, "manifest.profiles[0].dialect"},
		"panicking plugin": {func(t *testing.T) []byte { return fixture(t, "panic") }, CodeManifestInvalid, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := newTestRuntime(t, DefaultLimits, nil).Inspect(t.Context(), tc.module(t))
			wantError(t, err, tc.code, tc.field)
		})
	}
}

// A call past its limits fails with a typed error and takes nothing else
// down: the runtime keeps serving calls afterwards.
func TestCallPastItsLimitsFailsCleanly(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		behaviour, code string
		limits          Limits
	}{
		"time": {"loop", CodeTimedOut, Limits{Memory: 32 << 20, Time: time.Second}},
		// Generous time, so the memory limit is what stops the call even
		// under the race detector.
		"memory": {"allocate", CodeFailed, Limits{Memory: 16 << 20, Time: time.Minute}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newTestRuntime(t, tc.limits, nil)
			runaway, err := r.Load(t.Context(), fixture(t, tc.behaviour))
			if err != nil {
				t.Fatal(err)
			}
			wantError(t, runaway.Call(t.Context(), Call{Method: abi.MethodManifest}, nil), tc.code, "")
			if _, err = r.Inspect(t.Context(), fixture(t, "well-behaved")); err != nil {
				t.Fatalf("the runtime stopped serving after a failed call: %v", err)
			}
		})
	}
}

func TestCallerCancellationIsNotAPluginFailure(t *testing.T) {
	t.Parallel()
	r := newTestRuntime(t, DefaultLimits, nil)
	runaway, err := r.Load(t.Context(), fixture(t, "loop"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	if err = runaway.Call(ctx, Call{Method: abi.MethodManifest}, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the caller's deadline, got %v", err)
	}
}

func TestPluginOutputReachesTheLogRedacted(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	r := newTestRuntime(t, DefaultLimits, slog.New(slog.NewJSONHandler(&logged, nil)))
	for _, behaviour := range []string{"log", "panic"} {
		m, err := r.Load(t.Context(), fixture(t, behaviour))
		if err != nil {
			t.Fatal(err)
		}
		err = m.Call(t.Context(), Call{Method: abi.MethodManifest, Secrets: []string{fixtureSecret}}, nil)
		if behaviour == "panic" {
			if reported, ok := errors.AsType[*abi.Error](err); !ok || reported.Code != abi.CodeInternal {
				t.Fatalf("a panicking plugin reported %v", err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
	output := logged.String()
	if strings.Contains(output, fixtureSecret) {
		t.Fatalf("plugin output leaked a secret: %s", output)
	}
	for _, want := range []string{
		`"msg":"using [REDACTED]"`, `"plugin_attrs":{"credential":"[REDACTED]"}`,
		`"msg":"stderr [REDACTED]"`, `"stream":"stderr"`,
		`"msg":"plugin panicked"`, `"panic":"fixture panicked holding [REDACTED]"`,
		`"plugin_method":"manifest"`,
	} {
		if !strings.Contains(output, want) {
			t.Errorf("plugin log lacks %s:\n%s", want, output)
		}
	}
}
