// Package mcpservers registers and certifies project-owned upstream MCP servers.
// It is separate from the contract-generated management MCP endpoint.
package mcpservers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/routing"
)

var ErrCertification = errors.New("upstream MCP certification failed")
var toolName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}
type Catalog struct {
	Protocol string `json:"protocol_version"`
	Tools    []Tool `json:"tools"`
	Digest   string `json:"digest"`
}
type noSchemaNetwork struct{}

func (noSchemaNetwork) Load(string) (any, error) { return nil, ErrCertification }

// Certify negotiates a bounded Streamable HTTP session and captures a stable,
// validated tool catalog. It never invokes tools or runs schema network loaders.
func Certify(ctx context.Context, policy *egress.Policy, endpoint string, credential []byte) (*Catalog, error) {
	return CertifyTransport(ctx, policy, endpoint, credential, routing.MCPStreamableHTTP)
}

func CertifyTransport(ctx context.Context, policy *egress.Policy, endpoint string, credential []byte, transport routing.MCPTransport) (*Catalog, error) {
	return CertifyOptions(ctx, policy, endpoint, credential, TransportOptions{Transport: transport})
}

func CertifyOptions(ctx context.Context, policy *egress.Policy, endpoint string, credential []byte, opts TransportOptions) (*Catalog, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, err := DialTransport(ctx, policy, endpoint, credential, TransportOptions{ClientName: "openllmproxy-certifier", Timeout: 10 * time.Second, Transport: opts.Transport, Stdio: opts.Stdio, Executables: opts.Executables})
	if err != nil {
		return nil, ErrCertification
	}
	defer c.Close(ctx)
	return c.CertifyCatalog(ctx)
}

func (c *Transport) CertifyCatalog(ctx context.Context) (*Catalog, error) {
	credential := []byte(c.token)
	result := &Catalog{Protocol: c.Protocol(), Tools: []Tool{}}
	seen := map[string]bool{}
	cursors := map[string]bool{}
	cursor := ""
	for page := 0; page < 16; page++ {
		listing, listErr := c.ListTools(ctx, cursor)
		if listErr != nil || listing.Tools == nil {
			return nil, ErrCertification
		}
		for _, t := range listing.Tools {
			if !toolName.MatchString(t.Name) || seen[t.Name] || len(t.Description) > 4096 || len(t.InputSchema) > 64<<10 || len(result.Tools) >= 128 {
				return nil, ErrCertification
			}
			// Static authentication is never retained in upstream descriptions or
			// schema defaults, even when a hostile server reflects its bearer value.
			if c.token != "" && (strings.Contains(t.Name, c.token) || strings.Contains(t.Description, c.token) || bytes.Contains(t.InputSchema, credential)) {
				return nil, ErrCertification
			}
			if err := validateSchema(t.InputSchema); err != nil {
				return nil, ErrCertification
			}
			schema, _ := jsonschema.UnmarshalJSON(bytes.NewReader(t.InputSchema))
			if c.token != "" && reflectsCredential(schema, c.token) {
				return nil, ErrCertification
			}
			input, _ := json.Marshal(schema)
			seen[t.Name] = true
			result.Tools = append(result.Tools, Tool{t.Name, t.Description, input})
		}
		if listing.NextCursor == "" {
			sort.Slice(result.Tools, func(i, j int) bool { return result.Tools[i].Name < result.Tools[j].Name })
			encoded, _ := json.Marshal(result.Tools)
			if len(encoded) > 1<<20 {
				return nil, ErrCertification
			}
			sum := sha256.Sum256(encoded)
			result.Digest = hex.EncodeToString(sum[:])
			return result, nil
		}
		if len(listing.NextCursor) > 1024 || cursors[listing.NextCursor] {
			return nil, ErrCertification
		}
		cursors[listing.NextCursor] = true
		cursor = listing.NextCursor
	}
	return nil, ErrCertification
}
func validateSchema(raw json.RawMessage) error {
	if _, err := oif.ParseJSON(raw, oif.Limits{MaxBytes: 64 << 10, MaxDepth: 32, MaxNodes: 4096}); err != nil {
		return ErrCertification
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return ErrCertification
	}
	obj, ok := doc.(map[string]any)
	if !ok || obj["type"] != "object" {
		return ErrCertification
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(noSchemaNetwork{})
	if c.AddResource("https://olp.invalid/tool-schema", doc) != nil {
		return ErrCertification
	}
	_, err = c.Compile("https://olp.invalid/tool-schema")
	if err != nil {
		return ErrCertification
	}
	return nil
}

func reflectsCredential(value any, credential string) bool {
	switch v := value.(type) {
	case string:
		return strings.Contains(v, credential)
	case []any:
		for _, item := range v {
			if reflectsCredential(item, credential) {
				return true
			}
		}
	case map[string]any:
		for key, item := range v {
			if strings.Contains(key, credential) || reflectsCredential(item, credential) {
				return true
			}
		}
	}
	return false
}
