// Command catalogdrift compares the embedded reference catalog with the models
// each vendor lists today and prints the drift as Markdown. A vendor is
// compared when its API key is in the environment; the weekly catalog-drift
// workflow posts the report to an issue for a maintainer to review.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/catalog/drift"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/vendors"
	"github.com/tyk-swe/olp/internal/platform/egress"
)

type listing struct {
	vendor, key, query string
}

var listings = []listing{
	{vendor: "openai", key: "OPENAI_API_KEY"},
	{vendor: "anthropic", key: "ANTHROPIC_API_KEY", query: "?limit=1000"},
	{vendor: "google", key: "GEMINI_API_KEY", query: "?pageSize=1000"},
	{vendor: "mistral", key: "MISTRAL_API_KEY"},
	{vendor: "groq", key: "GROQ_API_KEY"},
	{vendor: "xai", key: "XAI_API_KEY"},
	{vendor: "deepseek", key: "DEEPSEEK_API_KEY"},
	{vendor: "cerebras", key: "CEREBRAS_API_KEY"},
	{vendor: "moonshot", key: "MOONSHOT_API_KEY"},
}

func (l listing) provider() connectors.Config {
	kind, _ := vendors.Kind(l.vendor)
	return connectors.Config{Kind: connectors.Kind(kind), VendorID: l.vendor, AuthMode: connectors.AuthAPIKey}
}

func (l listing) shape() (connectors.ModelListing, error) {
	shape, listed, err := l.provider().ModelListing()
	if err != nil {
		return connectors.ModelListing{}, err
	}
	if !listed {
		return connectors.ModelListing{}, fmt.Errorf("no reviewed model listing")
	}
	return shape, nil
}

func (l listing) url() (string, error) {
	contract, ok := vendors.Lookup(l.vendor)
	if !ok || contract.Endpoint == "" {
		return "", fmt.Errorf("no reviewed endpoint")
	}
	shape, err := l.shape()
	if err != nil {
		return "", err
	}
	return contract.Endpoint + shape.Path + l.query, nil
}

func main() {
	signed, err := catalog.Embedded()
	if err != nil {
		fmt.Fprintln(os.Stderr, "catalogdrift:", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 30 * time.Second}
	auth := connectors.NewAuth(&egress.Policy{}, os.Getenv)
	listed := map[string][]string{}
	for _, l := range listings {
		key := os.Getenv(l.key)
		if key == "" {
			continue
		}
		models, err := l.models(ctx, client, auth, key)
		if err != nil {
			fmt.Fprintf(os.Stderr, "catalogdrift: %s: %v\n", l.vendor, err)
			os.Exit(1)
		}
		listed[l.vendor] = models
	}
	now := time.Now()
	fmt.Print(drift.Compare(signed.Catalog, listed, now).Markdown(signed.Catalog, now))
}

func (l listing) models(ctx context.Context, client *http.Client, auth *connectors.Auth, key string) ([]string, error) {
	target, err := l.url()
	if err != nil {
		return nil, err
	}
	shape, err := l.shape()
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	if _, err := auth.Apply(ctx, request, l.provider(), []byte(key), nil); err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing answered %s", response.Status)
	}
	var document map[string][]map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, fmt.Errorf("listing is not a model list: %w", err)
	}
	var models []string
	for _, entry := range document[shape.Models] {
		if id, ok := entry[shape.ID].(string); ok {
			models = append(models, strings.TrimPrefix(id, shape.TrimIDPrefix))
		}
	}
	return models, nil
}
