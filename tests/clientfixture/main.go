// clientfixture serves the OLP inference surface in front of the scripted
// upstream of tests/clients, so real client releases run against the real
// gateway: bounded ingress, key authentication, route selection, credential
// injection, model rewriting and native streaming. Nothing here fabricates a
// success the gateway did not produce.
//
// It follows tests/sdkfixture, whose static runtime serves a pinned release
// without a database. Provider-retained state such as stored Responses needs
// the database-backed resource store and is therefore not served here.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/management"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/tests/clientfixture/scripted"
)

const (
	apiKey           = "olp_clients_key"
	stateAPIKey      = "olp_clients_state_key"
	restrictedAPIKey = "olp_clients_restricted_key"
	// credential is what the scripted upstream accepts; clients never see it.
	credential = "clients-upstream-credential"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// staticRuntime pins one release and two keys without a database.
type staticRuntime struct {
	release *runtime.Release
	keys    map[string]access.Authority
}

func (s *staticRuntime) Release() *runtime.Release { return s.release }

func (s *staticRuntime) RoutingInputs() *usage.RoutingInputs { return nil }

func (s *staticRuntime) Authenticate(secret string) (*access.Authority, error) {
	authority, ok := s.keys[secret]
	if !ok {
		return nil, runtime.ErrInvalidKey
	}
	return &authority, nil
}

func (s *staticRuntime) Eligibility(string) runtime.Eligibility { return runtime.Eligible }

// Secret serves the credentials the pinned release installed.
func (s *staticRuntime) Secret(_ context.Context, release *runtime.Release, credentialID string) ([]byte, int64, error) {
	if secret, ok := release.Credential(credentialID); ok {
		return secret, 0, nil
	}
	return nil, 0, runtime.ErrCredentialUnavailable
}

func (s *staticRuntime) NetworkSecret(ctx context.Context, release *runtime.Release, _, credentialID string) ([]byte, error) {
	secret, _, err := s.Secret(ctx, release, credentialID)
	return secret, err
}

// CredentialRefused ignores refusals: the fixture's credentials have no
// grants.
func (s *staticRuntime) CredentialRefused(string, int64) {}

func (s *staticRuntime) GrantGeneration(string) int64 { return 0 }

func run() error {
	path := os.Getenv("OLP_CLIENTS_METADATA")
	if path == "" {
		return fmt.Errorf("OLP_CLIENTS_METADATA is required")
	}
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer upstream.Close()
	fixture := scripted.New(scripted.Options{Credential: credential, ClientSecrets: []string{apiKey, stateAPIKey, restrictedAPIKey}})
	upstreamServer := &http.Server{Handler: fixture, ReadHeaderTimeout: 5 * time.Second}
	go upstreamServer.Serve(upstream)
	defer upstreamServer.Close()
	upstreamURL := "http://" + upstream.Addr().String()

	release, err := fixtureRelease(upstreamURL)
	if err != nil {
		return err
	}
	rt := &staticRuntime{release: release, keys: map[string]access.Authority{
		stateAPIKey:      {ID: uuid.NewString(), Issuer: uuid.NewString(), Policy: access.KeyPolicy{Scopes: []string{"inference", "models_read"}, AllowProviderState: true}},
		apiKey:           {ID: uuid.NewString(), Issuer: uuid.NewString(), Policy: access.KeyPolicy{Scopes: []string{"inference", "models_read"}}},
		restrictedAPIKey: {ID: uuid.NewString(), Issuer: uuid.NewString(), Policy: access.KeyPolicy{Scopes: []string{"inference", "models_read"}, AllowedRoutes: []string{"no-such-route"}}},
	}}
	policy := egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	// The gateway logs every request; a failing run shows warnings and errors,
	// and OLP_CLIENTS_LOG_LEVEL=info shows the requests.
	level := slog.LevelWarn
	if err := level.UnmarshalText([]byte(os.Getenv("OLP_CLIENTS_LOG_LEVEL"))); err != nil && os.Getenv("OLP_CLIENTS_LOG_LEVEL") != "" {
		return fmt.Errorf("OLP_CLIENTS_LOG_LEVEL: %w", err)
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	// Coding agents send whole conversations, so the body limit is generous.
	gw := gateway.New(rt, &policy, gateway.Config{MaxInFlight: 64, MaxBodyBytes: 16 << 20, MaxResponseBytes: 8 << 20, MaxEventBytes: 1 << 20}, log)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	mux := http.NewServeMux()
	management.Register(mux)
	gw.Register(mux)
	mux.HandleFunc("/", http.NotFound)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve(listener) }()
	defer server.Close()

	origin := "http://" + listener.Addr().String()
	metadata, err := json.Marshal(environment(origin, upstreamURL))
	if err != nil {
		return err
	}
	// Publish atomically only after binding; consumers never see partial JSON.
	if err := os.WriteFile(path+".tmp", metadata, 0600); err != nil {
		return err
	}
	defer os.Remove(path + ".tmp")
	if err := os.Rename(path+".tmp", path); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
	case err := <-errCh:
		return err
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return server.Shutdown(shutdown)
}

// backend is one scripted vendor.
type backend struct {
	name, kind, vendor, prefix, model string
	// surface is the client dialect the vendor speaks natively. A certified
	// capability names the client surface it serves, so a transformed route
	// reaches the vendor from every surface, by translation.
	surface string
	// embeddings is whether the vendor serves OpenAI-surface embeddings.
	embeddings bool
}

var allSurfaces = []string{"openai", "anthropic", "gemini"}

// capabilities certifies the backend's model for generation, token counting
// and embeddings on the given client surfaces.
func (b backend) capabilities(surfaces []string) []runtime.Capability {
	var out []runtime.Capability
	add := func(operation, surface, mode string) {
		out = append(out, runtime.Capability{Model: b.model, Operation: operation, Surface: surface, Mode: mode})
	}
	for _, surface := range surfaces {
		add("generation", surface, "unary")
		add("generation", surface, "streaming")
		add("token_count", surface, "unary")
	}
	// Embeddings are certified only on the OpenAI surface; native Gemini
	// embedding endpoints are served by profile on a strict route.
	if b.embeddings {
		add("embeddings", "openai", "unary")
	}
	return out
}

var backends = map[string]backend{
	"openai":    {name: "openai", kind: "openai", vendor: "openai", prefix: scripted.OpenAIPrefix, model: scripted.OpenAIModel, surface: "openai", embeddings: true},
	"anthropic": {name: "anthropic", kind: "anthropic", vendor: "anthropic", prefix: scripted.AnthropicPrefix, model: scripted.AnthropicModel, surface: "anthropic"},
	"gemini":    {name: "gemini", kind: "gemini", vendor: "gemini", prefix: scripted.GeminiPrefix, model: scripted.GeminiModel, surface: "gemini", embeddings: true},
}

// routeSpec is one published route, and so one model name for clients. A
// transformed route accepts any client dialect and translates it; a strict
// route preserves the native invocation and needs a provider per explicit
// versioned profile, one for each dialect the vendor speaks natively.
type routeSpec struct {
	// env names the model in the environment contract.
	env      string
	slug     string
	backend  string
	profiles []string
	// operations narrows the route to these; empty serves everything the
	// backend can.
	operations []string
}

var routeSpecs = []routeSpec{
	{env: "OPENAI", slug: "olp-openai", backend: "openai"},
	{env: "ANTHROPIC", slug: "olp-anthropic", backend: "anthropic"},
	{env: "GEMINI", slug: "olp-gemini", backend: "gemini"},
	{env: "OPENAI_STRICT", slug: "olp-openai-strict", backend: "openai", profiles: []string{"openai-chat", "openai-responses"}},
	{env: "ANTHROPIC_STRICT", slug: "olp-anthropic-strict", backend: "anthropic", profiles: []string{"anthropic-messages"}},
	{env: "GEMINI_STRICT", slug: "olp-gemini-strict", backend: "gemini", profiles: []string{"gemini-generation"}},
	// Native Gemini embedContent and batchEmbedContents are served only on a
	// strict route, by a profile each.
	{env: "GEMINI_EMBED_STRICT", slug: "olp-gemini-embed-strict", backend: "gemini", profiles: []string{"gemini-generation", "gemini-batch-embeddings"}, operations: []string{"embeddings"}},
	// Routes named after the vendor model a client library recognizes, as an
	// operator names a route after the model it replaces. LlamaIndex.TS enables
	// tool calling only for model names it knows.
	{env: "ANTHROPIC_CLAUDE", slug: "claude-sonnet-4-5", backend: "anthropic"},
	{env: "GEMINI_FLASH", slug: "gemini-2.5-flash", backend: "gemini"},
}

func fixtureRelease(upstreamURL string) (*runtime.Release, error) {
	now := time.Now().UTC()
	version := 1
	snapshot := &runtime.Snapshot{
		Generation: runtime.Generation{ID: uuid.NewString(), Ordinal: 1, ActivatedAt: now},
		Providers:  map[string]runtime.Provider{},
		Routes:     map[string]runtime.Route{},
	}
	secrets := map[string][]byte{}
	for _, spec := range routeSpecs {
		b := backends[spec.backend]
		mode := runtime.FidelityStrict
		profiles := spec.profiles
		if len(profiles) == 0 {
			mode, profiles = runtime.FidelityTransformed, []string{""}
		}
		routeID := uuid.NewString()
		route := runtime.Route{
			ID: routeID, Slug: spec.slug, OverallTimeout: 60000, MaxAttempts: 1, RoutingID: routeID,
			RevisionID: uuid.NewString(), Revision: 1, PublishedAt: now, Fidelity: runtime.RouteFidelity{Mode: mode},
		}
		for _, profile := range profiles {
			surfaces := allSurfaces
			if profile != "" {
				surfaces = []string{b.surface}
			}
			caps := slices.DeleteFunc(b.capabilities(surfaces), func(c runtime.Capability) bool {
				return len(spec.operations) > 0 && !slices.Contains(spec.operations, c.Operation)
			})
			if profile != "" {
				// A profile serves only the operations it declares.
				declared, err := connectors.LookupProfile(profile, connectors.ProfileRevision)
				if err != nil {
					return nil, err
				}
				caps = slices.DeleteFunc(caps, func(c runtime.Capability) bool { return !slices.Contains(declared.Operations, c.Operation) })
			}
			for _, c := range caps {
				if !slices.Contains(route.Operations, c.Operation) {
					route.Operations = append(route.Operations, c.Operation)
				}
			}
			providerID, credentialID, slotID, targetID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
			provider := runtime.Provider{
				ID: providerID, Name: "scripted " + b.name + " for " + spec.slug, Kind: b.kind, VendorID: b.vendor, Enabled: true,
				ActiveCredential: &credentialID, Capabilities: caps, RevisionID: uuid.NewString(),
				Endpoint: upstreamURL + b.prefix, AuthMode: "api_key", ProfileID: profile,
				// A transformed route accepts structured output only from a
				// model that declares it.
				Models: map[string]json.RawMessage{b.model: modelMetadata},
				Slots:  []runtime.Slot{{ID: slotID, Name: "default", Enabled: true, Weight: 1, CredentialID: &credentialID, CredentialVersion: &version}},
			}
			if profile != "" {
				provider.ProfileRevision = connectors.ProfileRevision
			}
			snapshot.Providers[providerID] = provider
			secrets[credentialID] = []byte(credential)
			route.Targets = append(route.Targets, runtime.Target{ID: targetID, ProviderID: providerID, ProviderModel: b.model, Weight: 1, Timeout: 55000, RoutingID: targetID})
		}
		snapshot.Routes[spec.slug] = route
	}
	return runtime.NewRelease(uuid.NewString(), 1, snapshot, secrets)
}

// modelMetadata declares the optional parameters every scripted model takes.
// It leaves context and output limits unknown, so admission never excludes a
// target on an estimate.
var modelMetadata = json.RawMessage(`{"supported_parameters":["response_format","tools","tool_choice","parallel_tool_calls","temperature","top_p","top_k","max_tokens","max_completion_tokens","max_output_tokens","stop","stop_sequences","seed","reasoning","reasoning_effort","thinking"]}`)

// environment is the contract with client processes: each member becomes an
// environment variable of the suite. tests/clients/README.md documents it.
func environment(origin, upstreamURL string) map[string]string {
	env := map[string]string{
		"OLP_CLIENTS_ORIGIN":              origin,
		"OLP_CLIENTS_API_KEY":             apiKey,
		"OLP_CLIENTS_STATE_API_KEY":       stateAPIKey,
		"OLP_CLIENTS_RESTRICTED_API_KEY":  restrictedAPIKey,
		"OLP_CLIENTS_OPENAI_BASE_URL":     origin + "/v1",
		"OLP_CLIENTS_ANTHROPIC_BASE_URL":  origin + "/anthropic",
		"OLP_CLIENTS_GEMINI_BASE_URL":     origin + "/gemini",
		"OLP_CLIENTS_UPSTREAM_URL":        upstreamURL,
		"OLP_CLIENTS_DEFAULT_REPLY":       scripted.DefaultReply,
		"OLP_CLIENTS_TOOL_RESULTS_PREFIX": scripted.ToolResultsPrefix,
	}
	for _, spec := range routeSpecs {
		env["OLP_CLIENTS_MODEL_"+spec.env] = spec.slug
	}
	for _, b := range backends {
		env["OLP_CLIENTS_UPSTREAM_MODEL_"+strings.ToUpper(b.name)] = b.model
	}
	return env
}

// This fixture has no asynchronous publication or external database.
func (*staticRuntime) CheckRouteFidelity(context.Context, runtime.Route) error { return nil }
