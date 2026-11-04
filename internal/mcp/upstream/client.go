package upstream

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
	"github.com/tyk-swe/olp/internal/routing"
)

var ErrCertification = errors.New("upstream MCP certification failed")
var toolName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

type Catalog struct {
	Protocol string            `json:"protocol_version"`
	Tools    []routing.MCPTool `json:"tools"`
	Digest   string            `json:"digest"`
}

func CertifyOptions(ctx context.Context, policy *egress.Policy, endpoint string, credential []byte, opts TransportOptions) (*Catalog, error) {
	certifyContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, err := DialTransport(certifyContext, policy, endpoint, credential, TransportOptions{ClientName: "openllmproxy-certifier", Timeout: 10 * time.Second, Transport: opts.Transport, Stdio: opts.Stdio, Executables: opts.Executables})
	if err != nil {
		return nil, ErrCertification
	}
	defer c.Close(certifyContext)
	return CertifyCatalog(certifyContext, c, credential)
}

func CertifyCatalog(ctx context.Context, upstream Upstream, credential []byte) (*Catalog, error) {
	token := string(credential)
	result := &Catalog{Protocol: upstream.Protocol(), Tools: []routing.MCPTool{}}
	seen := map[string]bool{}
	cursors := map[string]bool{}
	cursor := ""
	for page := 0; page < 16; page++ {
		listing, listErr := upstream.ListTools(ctx, cursor)
		if listErr != nil || listing.Tools == nil {
			return nil, ErrCertification
		}
		for _, t := range listing.Tools {
			if !toolName.MatchString(t.Name) || seen[t.Name] || len(t.Description) > 4096 || len(t.InputSchema) > 64<<10 || len(result.Tools) >= 128 {
				return nil, ErrCertification
			}
			// Static authentication is never retained in upstream descriptions or
			// schema defaults, even when a hostile server reflects its bearer value.
			if token != "" && (strings.Contains(t.Name, token) || strings.Contains(t.Description, token) || bytes.Contains(t.InputSchema, credential)) {
				return nil, ErrCertification
			}
			if err := validateSchema(t.InputSchema); err != nil {
				return nil, ErrCertification
			}
			schema, _ := jsonschema.UnmarshalJSON(bytes.NewReader(t.InputSchema))
			if token != "" && reflectsCredential(schema, token) {
				return nil, ErrCertification
			}
			input, _ := json.Marshal(schema)
			seen[t.Name] = true
			result.Tools = append(result.Tools, routing.MCPTool{Name: t.Name, Description: t.Description, InputSchema: input})
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
	if err := routing.ValidateToolSchema(raw); err != nil {
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
